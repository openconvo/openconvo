package syncer

import (
	"context"
	"fmt"
	"time"

	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/jobs"
)

const (
	// reconcileOverlap re-reads a little history before the last known
	// sync point so nothing on the boundary is missed.
	reconcileOverlap = 15 * time.Minute
	// reconcileDefaultWindow bounds the window when a channel has never
	// reconciled before.
	reconcileDefaultWindow = 24 * time.Hour
	// reconcileDeletionGrace protects messages younger than this from
	// deletion detection: they may still be in flight.
	reconcileDeletionGrace = 5 * time.Minute
)

// EnqueueReconcile schedules a reconciliation pass for a channel.
func (s *Syncer) EnqueueReconcile(ctx context.Context, channelID string) error {
	_, err := s.queue.Enqueue(ctx, JobReconcile, channelJobPayload{ChannelID: channelID},
		jobs.WithDedupeKey(JobReconcile+":"+channelID))
	return err
}

// HandleReconcile re-fetches recent history and repairs the archive:
// gaps are filled, missed edits applied, and deletions inside the
// fetched window tombstoned. The Gateway is never trusted to be a
// perfect event stream.
func (s *Syncer) HandleReconcile(ctx context.Context, job *jobs.Job) error {
	var payload channelJobPayload
	if err := job.UnmarshalPayload(&payload); err != nil {
		return err
	}
	logger := s.logger.With("channel_id", payload.ChannelID)

	channel, ok, err := s.store.GetChannel(ctx, payload.ChannelID)
	if err != nil || !ok {
		return err
	}
	enabled, err := s.effectiveEnabled(ctx, channel)
	if err != nil || !enabled {
		return err
	}
	state, err := s.store.GetOrCreateSyncState(ctx, channel.ID)
	if err != nil {
		return err
	}
	if !state.BackfillComplete {
		return nil // backfill owns this channel's state until it completes
	}
	if containerOfThreadsOnly(channel.Kind) {
		return s.store.TouchSynced(ctx, channel.ID) // no direct messages
	}

	window := time.Now().Add(-reconcileDefaultWindow)
	if state.LastSyncedAt != nil {
		window = state.LastSyncedAt.Add(-reconcileOverlap)
	}

	fetched := map[string]bool{}
	var oldestFetched time.Time
	newestID := ""
	before := ""
	for {
		raws, err := s.client.ListChannelMessages(ctx, channel.ExternalID, before, backfillPageSize)
		if err != nil {
			if s.recordPermanentSyncError(ctx, channel.ID, err) {
				logger.Warn("reconcile lost access to channel", "error", err)
				return nil
			}
			return err
		}
		if len(raws) == 0 {
			break
		}
		pastWindow := false
		for _, raw := range raws {
			// Presence is recorded before parsing: Discord returned this
			// message, so it is alive whatever its payload looks like,
			// and deletion detection below reads only this set.
			if id := externalID(raw); id != "" {
				fetched[id] = true
			}
			msg, err := discord.NormalizeMessage(raw)
			if err != nil {
				return fmt.Errorf("normalize reconcile message before %q: %w", before, err)
			}
			if _, err := s.ingester.ApplyMessage(ctx, msg); err != nil {
				return err
			}
			if newestID == "" {
				newestID = msg.ExternalID
			}
			oldestFetched = msg.CreatedAt
			if msg.CreatedAt.Before(window) {
				pastWindow = true
			}
		}
		if pastWindow || len(raws) < backfillPageSize {
			break
		}
		nextBefore := externalID(raws[len(raws)-1])
		if nextBefore == "" || nextBefore == before {
			return fmt.Errorf("reconcile could not advance pagination before %q", before)
		}
		before = nextBefore
	}

	// Deletion detection, strictly inside the fetched window: anything
	// archived in that range that Discord no longer returns is gone. A
	// pass that parsed nothing has no lower bound for that range, so it
	// detects no deletion rather than guessing at one.
	if !oldestFetched.IsZero() {
		archivedIDs, err := s.store.ListArchivedExternalIDsSince(ctx, channel.ID, oldestFetched)
		if err != nil {
			return err
		}
		for _, id := range archivedIDs {
			if fetched[id] {
				continue
			}
			if ts := discord.SnowflakeTime(id); time.Since(ts) < reconcileDeletionGrace {
				continue
			}
			if _, err := s.ingester.ApplyMessageDelete(ctx, channel.ExternalID, id); err != nil {
				return err
			}
			logger.Info("reconcile detected deletion", "external_id", id)
		}
		if err := s.store.SetNewestSynced(ctx, channel.ID, newestID); err != nil {
			return err
		}
	}
	return s.store.TouchSynced(ctx, channel.ID)
}
