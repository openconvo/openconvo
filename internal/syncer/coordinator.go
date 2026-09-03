package syncer

import (
	"context"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
)

const (
	// coordinatorTick is how often the coordinator looks for channels
	// due for reconciliation.
	coordinatorTick = 15 * time.Minute
	// reconcileEvery is how stale a synced channel may get before it is
	// reconciled again.
	reconcileEvery = 6 * time.Hour
)

// RegisterHandlers attaches the sync job handlers to a worker.
func (s *Syncer) RegisterHandlers(w *jobs.Worker) {
	w.Register(JobBackfill, s.HandleBackfill)
	w.Register(JobReconcile, s.HandleReconcile)
}

// ChannelToggled reacts to the operator enabling or disabling archiving
// for a channel. Disabling retains what was already archived; purging is
// a separate, deliberate operator action.
func (s *Syncer) ChannelToggled(ctx context.Context, channelID string, enabled bool) error {
	// Enablement affects the threads under the channel too, so the
	// ingest cache is reset wholesale.
	s.ingester.InvalidateAllChannels()

	state, err := s.store.GetOrCreateSyncState(ctx, channelID)
	if err != nil {
		return err
	}
	if !enabled {
		return s.store.SetSyncStatus(ctx, channelID, archive.SyncStatusDisabled)
	}
	if state.BackfillComplete {
		return s.store.TouchSynced(ctx, channelID)
	}
	if state.Status == archive.SyncStatusDisabled || state.Status == archive.SyncStatusError {
		if err := s.store.SetSyncStatus(ctx, channelID, archive.SyncStatusPending); err != nil {
			return err
		}
	}
	return s.EnqueueBackfill(ctx, channelID)
}

// ReconcileAll schedules reconciliation for every synced channel, e.g.
// after a gateway session could not resume and events may have been lost.
func (s *Syncer) ReconcileAll(ctx context.Context) {
	rows, err := s.store.SyncOverview(ctx)
	if err != nil {
		s.logger.Error("reconcile-all: list channels", "error", err)
		return
	}
	for _, row := range rows {
		if !row.BackfillComplete {
			continue
		}
		if err := s.EnqueueReconcile(ctx, row.ChannelID); err != nil {
			s.logger.Error("reconcile-all: enqueue", "channel_id", row.ChannelID, "error", err)
		}
	}
}

// EnqueueDueBackfills schedules a backfill for every archived channel
// whose history is still incomplete.
//
// It covers the threads under an enabled channel as well as the channel
// itself, which matters for threads that surface after their parent
// finished importing: the Gateway announces such a thread but never
// replays the messages it already holds, so without this sweep that
// history would stay missing. A channel whose last attempt failed
// permanently (lost access) is retried here too, so fixing the
// permission on Discord is enough to resume.
//
// Returns how many channels were due; a dedupe key suppresses the
// enqueue for any that already have a backfill pending or running.
func (s *Syncer) EnqueueDueBackfills(ctx context.Context) (int, error) {
	rows, err := s.store.SyncOverview(ctx)
	if err != nil {
		return 0, err
	}
	due := 0
	for _, row := range rows {
		if row.BackfillComplete {
			continue
		}
		if err := s.EnqueueBackfill(ctx, row.ChannelID); err != nil {
			return due, err
		}
		due++
	}
	return due, nil
}

// EnqueueDueReconciles schedules reconciliation for synced channels
// whose last pass is older than reconcileEvery. Returns how many
// channels were due.
func (s *Syncer) EnqueueDueReconciles(ctx context.Context) (int, error) {
	rows, err := s.store.SyncOverview(ctx)
	if err != nil {
		return 0, err
	}
	due := 0
	for _, row := range rows {
		if !row.BackfillComplete {
			continue
		}
		if row.LastSyncedAt != nil && time.Since(*row.LastSyncedAt) < reconcileEvery {
			continue
		}
		if err := s.EnqueueReconcile(ctx, row.ChannelID); err != nil {
			return due, err
		}
		due++
	}
	return due, nil
}

// Run performs startup recovery, then keeps sync work scheduled on a
// ticker until ctx ends. Both sweeps are idempotent: dedupe keys make
// repeating them harmless.
func (s *Syncer) Run(ctx context.Context) {
	// Startup recovery: whatever a crash or shutdown left unfinished.
	s.scheduleBackfills(ctx, "startup: ")

	ticker := time.NewTicker(coordinatorTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scheduleBackfills(ctx, "")
			if n, err := s.EnqueueDueReconciles(ctx); err != nil {
				s.logger.Error("schedule reconciles", "error", err)
			} else if n > 0 {
				s.logger.Info("scheduled reconciliations", "count", n)
			}
		}
	}
}

func (s *Syncer) scheduleBackfills(ctx context.Context, phase string) {
	n, err := s.EnqueueDueBackfills(ctx)
	if err != nil {
		s.logger.Error(phase+"schedule backfills", "error", err)
		return
	}
	if n > 0 {
		s.logger.Info(phase+"scheduled backfills", "count", n)
	}
}
