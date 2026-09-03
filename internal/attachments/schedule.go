package attachments

import (
	"context"
	"time"

	"github.com/openconvo/openconvo/internal/jobs"
)

const (
	// sweepInterval is how often pending attachments are looked for. A
	// minute is well inside the lifetime of a signed URL, and keeps a
	// newly archived file's download close behind the message.
	sweepInterval = time.Minute
	// sweepBatch bounds how many jobs one pass enqueues, so a large
	// backlog arrives in waves rather than as one enormous burst.
	sweepBatch = 500

	// gcInterval is how often orphaned blobs are reclaimed.
	gcInterval = time.Hour
)

// enqueueDownload schedules one attachment and reports whether a job row
// was actually created. It stays unexported on purpose: EnqueueDuePending
// is the only entry point, because that is where the "downloads are
// enabled" gate lives, and an exported sibling without the gate is an
// invitation to reopen it.
//
// A false means the dedupe key suppressed the enqueue because one is
// already pending or running — normal while a download is in flight, and
// not something to count as newly scheduled. The payload type is
// downloadPayload from download.go — one definition, so the producer and
// the handler cannot drift apart.
func (d *Downloader) enqueueDownload(ctx context.Context, attachmentID string) (bool, error) {
	id, err := d.queue.Enqueue(ctx, JobDownload,
		downloadPayload{AttachmentID: attachmentID},
		jobs.WithDedupeKey(JobDownload+":"+attachmentID),
		jobs.WithMaxAttempts(downloadMaxAttempts))
	return id != "", err
}

// EnqueueDuePending schedules downloads for attachments whose file is
// not stored yet, and returns how many jobs were newly created — not how
// many attachments were found pending, since one of them already has a
// job outstanding whenever a previous sweep's download is still running
// (sweepInterval is well under downloadTimeout, so that's routine, not
// an edge case). It is the single entry point for both newly archived
// files and the backlog of an archive that predates the pipeline.
func (d *Downloader) EnqueueDuePending(ctx context.Context) (int, error) {
	if !d.enabled {
		return 0, nil
	}
	// A storage-side failure leaves its attachment pending by design, so
	// re-offering the whole batch a minute later achieves nothing while
	// the disk is still full. One line per skipped sweep, never one per
	// attachment: the operator has to be able to read this log.
	if until, cooling := d.storageCoolingUntil(time.Now()); cooling {
		d.logger.Warn("attachment storage failed recently; sweep paused",
			"resumes_at", until.UTC().Format(time.RFC3339))
		return 0, nil
	}
	pending, err := d.store.ListPendingAttachments(ctx, sweepBatch)
	if err != nil {
		return 0, err
	}
	scheduled := 0
	for _, att := range pending {
		enqueued, err := d.enqueueDownload(ctx, att.ID)
		if err != nil {
			return scheduled, err
		}
		if enqueued {
			scheduled++
		}
	}
	return scheduled, nil
}

// EnqueueGC schedules a reclamation pass.
func (d *Downloader) EnqueueGC(ctx context.Context) error {
	_, err := d.queue.Enqueue(ctx, JobGC, struct{}{},
		jobs.WithDedupeKey(JobGC),
		jobs.WithMaxAttempts(gcMaxAttempts))
	return err
}

// Run keeps attachment work scheduled until ctx ends. Both sweeps are
// idempotent: dedupe keys make repeating them harmless.
func (d *Downloader) Run(ctx context.Context) {
	switch {
	case d.enabled:
		d.logger.Info("attachment downloads enabled", "max_bytes", d.maxBytes)
	case d.optedIn:
		// Configured on, but impossible: name the actual reason rather
		// than telling the operator to set a variable they already set.
		d.logger.Warn("attachment downloads are enabled but cannot run: no Discord token, so expired file URLs cannot be refreshed")
	default:
		d.logger.Info("attachment downloads disabled; set OPENCONVO_ATTACHMENTS_ENABLED=true to store files")
	}

	d.sweep(ctx)
	// Reclaim at startup too: an installation restarted more often than
	// hourly would otherwise never collect anything.
	if err := d.EnqueueGC(ctx); err != nil {
		d.logger.Error("schedule blob reclamation", "error", err)
	}

	downloads := time.NewTicker(sweepInterval)
	defer downloads.Stop()
	collect := time.NewTicker(gcInterval)
	defer collect.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-downloads.C:
			d.sweep(ctx)
		case <-collect.C:
			if err := d.EnqueueGC(ctx); err != nil {
				d.logger.Error("schedule blob reclamation", "error", err)
			}
		}
	}
}

func (d *Downloader) sweep(ctx context.Context) {
	n, err := d.EnqueueDuePending(ctx)
	if err != nil {
		d.logger.Error("schedule attachment downloads", "error", err)
		return
	}
	if n > 0 {
		d.logger.Info("scheduled attachment downloads", "count", n)
	}
}
