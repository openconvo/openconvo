// Package attachments downloads the files attached to archived messages
// into content-addressed blob storage, and reclaims blobs that nothing
// references any more.
//
// Downloading is deliberately a background job rather than part of
// ingestion: files are large, remote and slow, and none of that belongs
// in the path that records what was said.
package attachments

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/storage"
)

// Job kinds.
const (
	JobDownload = "attachment.download"
	JobGC       = "storage.gc"
)

// DefaultMaxBytes is the per-file ceiling: above Discord's 25 MB
// free-tier upload limit, below the 500 MB Nitro one, so it captures
// essentially every real file without letting one upload fill a small
// disk.
const DefaultMaxBytes int64 = 100 << 20

const (
	// downloadMaxAttempts spans the window in which trouble is allowed to
	// be transient. Once it is exhausted, a non-terminal error becomes a
	// permanent 'failed' verdict the sweep never re-offers, so the window
	// has to be wide enough to outlast the realistic causes: a CDN having
	// a bad minute, an uplink blip, a DNS hiccup. With jobs.Backoff (3s,
	// 6s, 12s ... capped at 10 min), ten attempts span about 23 minutes;
	// five spanned 45 seconds, which is not a wobble, it is a coin flip.
	downloadMaxAttempts = 10
	downloadTimeout     = 10 * time.Minute

	// storageFailureCooldown pauses the sweep after a storage-side
	// failure. Those failures — a full disk above all — leave the
	// attachment pending on purpose, so without a pause the next sweep
	// re-enqueues the whole batch a minute later and the pipeline spins
	// hot for as long as the disk stays full: hundreds of job rows and
	// thousands of log lines a minute, none of which fix anything. Five
	// minutes turns that into a heartbeat, and costs a stalled download
	// at most five minutes once the operator frees space.
	storageFailureCooldown = 5 * time.Minute

	// gcMaxAttempts is lower than downloadMaxAttempts: reclamation only
	// touches local storage and the database, so a failure is more
	// likely a structural problem an operator must fix than a transient
	// one more retries would clear.
	gcMaxAttempts = 3
)

// URLRefresher obtains working download URLs for source URLs whose
// signatures have expired.
//
// It is an interface here rather than a discord import so the pipeline
// stays source-agnostic; *discord.Client satisfies it directly.
type URLRefresher interface {
	RefreshAttachmentURLs(ctx context.Context, urls []string) (map[string]string, error)
}

// Options configures the pipeline.
type Options struct {
	// Enabled gates downloading. Reclamation runs regardless: it
	// enforces deletion, which is not optional.
	Enabled bool
	// MaxBytes is the per-file ceiling; zero means DefaultMaxBytes.
	MaxBytes int64
}

// Downloader runs the attachment jobs.
type Downloader struct {
	store     *archive.Store
	blobs     storage.Store
	queue     *jobs.Queue
	refresher URLRefresher
	http      *http.Client
	// optedIn is what the operator configured; enabled is what the
	// pipeline can actually do. They differ when downloads are switched
	// on with no Discord token, and the difference is what status and the
	// startup log must report.
	optedIn  bool
	enabled  bool
	maxBytes int64
	logger   *slog.Logger

	// mu guards lastStorageFailure: the download handler records it from
	// a worker goroutine while the sweep reads it from Run's.
	mu                 sync.Mutex
	lastStorageFailure time.Time
}

// New creates a Downloader. A nil refresher disables downloading, since
// an expired URL could never be replaced.
func New(store *archive.Store, blobs storage.Store, queue *jobs.Queue, refresher URLRefresher, opts Options, logger *slog.Logger) *Downloader {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Downloader{
		store:     store,
		blobs:     blobs,
		queue:     queue,
		refresher: refresher,
		// No overall client timeout: a large file on a slow link is not
		// an error. Per-request deadlines come from the job context.
		http:     &http.Client{},
		optedIn:  opts.Enabled,
		enabled:  opts.Enabled && refresher != nil,
		maxBytes: maxBytes,
		logger:   logger.With("component", "attachments"),
	}
}

// Enabled reports whether downloading actually happens, which is not the
// same as what the operator configured: without a refresher an expired
// URL could never be replaced, so downloading stays off. Status surfaces
// must report this rather than the configuration flag, or they promise
// downloads that cannot happen.
func (d *Downloader) Enabled() bool { return d.enabled }

// noteStorageFailure records that storage rejected a write, which pauses
// the sweep for storageFailureCooldown.
func (d *Downloader) noteStorageFailure(at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastStorageFailure = at
}

// storageCoolingUntil returns the instant the sweep may resume, and
// whether it is still in the future.
func (d *Downloader) storageCoolingUntil(now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastStorageFailure.IsZero() {
		return time.Time{}, false
	}
	until := d.lastStorageFailure.Add(storageFailureCooldown)
	return until, until.After(now)
}

// RegisterHandlers attaches the attachment jobs to a worker.
func (d *Downloader) RegisterHandlers(w *jobs.Worker) {
	w.Register(JobDownload, d.HandleDownload)
	w.Register(JobGC, d.HandleGC)
}
