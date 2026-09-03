// Package syncer orchestrates Discord synchronization: resumable
// historical backfill and reconciliation, both running as PostgreSQL-
// backed jobs so they survive restarts and retry with backoff.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/ingest"
	"github.com/openconvo/openconvo/internal/jobs"
)

// Job kinds.
const (
	JobBackfill  = "discord.backfill"
	JobReconcile = "discord.reconcile"
)

const (
	backfillPageSize = 100
	// backfillPagePause is the deliberate pacing between history pages:
	// adding OpenConvo to a large server must never be abusive, and
	// correctness matters more than import speed. This is the only
	// deliberate delay outside the REST client's rate limiter.
	backfillPagePause = 750 * time.Millisecond
)

// Syncer owns backfill and reconciliation.
type Syncer struct {
	store    *archive.Store
	queue    *jobs.Queue
	client   *discord.Client
	ingester *ingest.Ingester
	logger   *slog.Logger
}

// New creates a Syncer.
func New(store *archive.Store, queue *jobs.Queue, client *discord.Client, ingester *ingest.Ingester, logger *slog.Logger) *Syncer {
	return &Syncer{
		store: store, queue: queue, client: client, ingester: ingester,
		logger: logger.With("component", "sync"),
	}
}

type channelJobPayload struct {
	ChannelID string `json:"channel_id"`
}

// EnqueueBackfill schedules a channel backfill; duplicates are
// suppressed while one is pending or running.
func (s *Syncer) EnqueueBackfill(ctx context.Context, channelID string) error {
	_, err := s.queue.Enqueue(ctx, JobBackfill, channelJobPayload{ChannelID: channelID},
		jobs.WithDedupeKey(JobBackfill+":"+channelID))
	return err
}

// HandleBackfill imports a channel's full history, newest→oldest, with
// a checkpoint after every page so any interruption resumes instead of
// restarting.
func (s *Syncer) HandleBackfill(ctx context.Context, job *jobs.Job) error {
	var payload channelJobPayload
	if err := job.UnmarshalPayload(&payload); err != nil {
		return err
	}
	logger := s.logger.With("channel_id", payload.ChannelID)

	channel, ok, err := s.store.GetChannel(ctx, payload.ChannelID)
	if err != nil {
		return err
	}
	if !ok {
		logger.Warn("backfill for unknown channel; skipping")
		return nil
	}

	enabled, err := s.effectiveEnabled(ctx, channel)
	if err != nil {
		return err
	}
	state, err := s.store.GetOrCreateSyncState(ctx, channel.ID)
	if err != nil {
		return err
	}
	if !enabled {
		// The operator turned the channel off between enqueue and
		// execution: record that, and touch nothing on Discord.
		return s.store.SetSyncStatus(ctx, channel.ID, archive.SyncStatusDisabled)
	}

	if !state.BackfillComplete {
		if err := s.store.SetSyncStatus(ctx, channel.ID, archive.SyncStatusImporting); err != nil {
			return err
		}
		// Forum and media channels hold threads, not messages of their own.
		if !containerOfThreadsOnly(channel.Kind) {
			interrupted, err := s.backfillMessages(ctx, logger, channel, state)
			if err != nil {
				return err
			}
			if interrupted {
				// Permanent source failures and operator disablement are both
				// recorded on the channel. Neither may checkpoint skipped data
				// or mark this backfill complete.
				return nil
			}
		}
	}

	blocked, err := s.enqueueThreadBackfills(ctx, logger, channel)
	if err != nil {
		return err
	}
	if blocked {
		return nil
	}
	if _, err := s.store.ResolveReplyLinks(ctx, channel.ID); err != nil {
		return err
	}
	if err := s.store.MarkBackfillComplete(ctx, channel.ID); err != nil {
		return err
	}
	logger.Info("backfill complete", "channel", channel.Name)
	return nil
}

// backfillMessages pages through history until exhausted. It reports
// interrupted=true when access was lost permanently (403/404) or the operator
// disabled the channel while it was running. Both leave the cursor at the last
// fully archived page.
func (s *Syncer) backfillMessages(ctx context.Context, logger *slog.Logger, channel archive.Channel, state archive.SyncState) (interrupted bool, err error) {
	before := ""
	if state.OldestExternalID != nil {
		before = *state.OldestExternalID
	}
	firstPage := state.NewestExternalID == nil && before == ""

	for {
		enabled, err := s.effectiveEnabled(ctx, channel)
		if err != nil {
			return false, err
		}
		if !enabled {
			if err := s.store.SetSyncStatus(ctx, channel.ID, archive.SyncStatusDisabled); err != nil {
				return false, err
			}
			return true, nil
		}
		raws, err := s.client.ListChannelMessages(ctx, channel.ExternalID, before, backfillPageSize)
		if err != nil {
			if s.recordPermanentSyncError(ctx, channel.ID, err) {
				logger.Warn("backfill lost access to channel", "error", err)
				return true, nil
			}
			return false, fmt.Errorf("list messages before %q: %w", before, err)
		}
		if len(raws) == 0 {
			return false, nil
		}

		for _, raw := range raws {
			msg, err := discord.NormalizeMessage(raw)
			if err != nil {
				return false, fmt.Errorf("normalize backfill message before %q: %w", before, err)
			}
			applied, err := s.ingester.ApplyMessage(ctx, msg)
			if err != nil {
				return false, err
			}
			if !applied {
				enabled, err := s.effectiveEnabled(ctx, channel)
				if err != nil {
					return false, err
				}
				if !enabled {
					if err := s.store.SetSyncStatus(ctx, channel.ID, archive.SyncStatusDisabled); err != nil {
						return false, err
					}
					return true, nil
				}
				return false, fmt.Errorf("backfill message %s was not applied to enabled channel %s", msg.ExternalID, channel.ExternalID)
			}
		}

		newestID, oldestID := externalID(raws[0]), externalID(raws[len(raws)-1])
		if firstPage && newestID != "" {
			if err := s.store.SetNewestSynced(ctx, channel.ID, newestID); err != nil {
				return false, err
			}
			firstPage = false
		}
		if err := s.store.UpdateBackfillCheckpoint(ctx, channel.ID, oldestID); err != nil {
			return false, err
		}
		before = oldestID

		if len(raws) < backfillPageSize {
			return false, nil
		}
		select {
		case <-time.After(backfillPagePause):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// enqueueThreadBackfills discovers this channel's threads (archived via
// REST, active via previously ingested Gateway data) and schedules them.
// Threads have no threads of their own, so they are skipped entirely.
func (s *Syncer) enqueueThreadBackfills(ctx context.Context, logger *slog.Logger, channel archive.Channel) (blocked bool, err error) {
	if isThread(channel) {
		return false, nil
	}

	// Archived public threads from REST.
	before := ""
	for {
		raws, hasMore, err := s.client.ListPublicArchivedThreads(ctx, channel.ExternalID, before)
		if err != nil {
			if s.recordPermanentSyncError(ctx, channel.ID, err) {
				logger.Warn("thread discovery lost access to channel", "error", err)
				return true, nil
			}
			return false, err
		}
		lastTimestamp := ""
		for _, raw := range raws {
			th, err := discord.NormalizeChannel(raw)
			if err != nil {
				return false, fmt.Errorf("normalize archived thread: %w", err)
			}
			stored, err := s.ingester.ApplyChannel(ctx, "", th)
			if err != nil {
				return false, err
			}
			if err := s.EnqueueBackfill(ctx, stored.ID); err != nil {
				return false, err
			}
			lastTimestamp = th.ThreadArchiveTimestamp
		}
		if !hasMore || lastTimestamp == "" {
			break
		}
		before = lastTimestamp
	}

	// Threads already known from the Gateway (active threads).
	siblings, err := s.store.ListChannels(ctx, channel.CommunityID)
	if err != nil {
		return false, err
	}
	for _, sibling := range siblings {
		if sibling.ParentChannelID != nil && *sibling.ParentChannelID == channel.ID {
			if err := s.EnqueueBackfill(ctx, sibling.ID); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// effectiveEnabled is a channel's own flag, or the parent's for threads.
func (s *Syncer) effectiveEnabled(ctx context.Context, channel archive.Channel) (bool, error) {
	// The caller may hold a channel snapshot for the duration of a long-running
	// job. Re-read the selection gate each time so an operator's disablement is
	// observed before another page is fetched or checkpointed.
	current, ok, err := s.store.GetChannel(ctx, channel.ID)
	if err != nil || !ok {
		return false, err
	}
	if current.ArchiveEnabled {
		return true, nil
	}
	if current.ParentChannelID == nil {
		return false, nil
	}
	parent, ok, err := s.store.GetChannel(ctx, *current.ParentChannelID)
	if err != nil {
		return false, err
	}
	return ok && parent.ArchiveEnabled, nil
}

// recordPermanentSyncError reports true for API errors retrying cannot
// fix (403 lost access, 404 gone) after recording them on the channel.
func (s *Syncer) recordPermanentSyncError(ctx context.Context, channelID string, err error) bool {
	var apiErr *discord.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status != 403 && apiErr.Status != 404 {
		return false
	}
	if recErr := s.store.SetSyncError(ctx, channelID, apiErr.Error()); recErr != nil {
		s.logger.Error("record sync error", "error", recErr)
	}
	return true
}

// containerOfThreadsOnly reports kinds that hold threads rather than
// messages of their own.
func containerOfThreadsOnly(kind string) bool {
	return kind == "forum" || kind == "media"
}

// isThread reports whether a channel is itself a thread.
func isThread(channel archive.Channel) bool {
	return channel.ParentChannelID != nil &&
		(channel.Kind == "thread" || channel.Kind == "private_thread")
}

func externalID(raw json.RawMessage) string {
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.ID
}
