package attachments_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/attachments"
	"github.com/openconvo/openconvo/internal/jobs"
)

func TestEnqueueDuePendingSchedulesEachAttachmentOnce(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.addAttachment(t, "m1", "https://cdn.example/1.png?ex=ffffffff", 10)
	f.addAttachment(t, "m2", "https://cdn.example/2.png?ex=ffffffff", 20)

	n, err := d.EnqueueDuePending(f.ctx)
	if err != nil || n != 2 {
		t.Fatalf("EnqueueDuePending = %d, err %v; want 2", n, err)
	}
	if got := f.countJobs(t, attachments.JobDownload); got != 2 {
		t.Errorf("jobs = %d, want 2", got)
	}

	// The second sweep finds the same rows, but every job is already
	// queued, so nothing new is scheduled and the count must say so.
	n, err = d.EnqueueDuePending(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second sweep scheduled %d, want 0 — the count must report insertions, not candidates", n)
	}
	if got := f.countJobs(t, attachments.JobDownload); got != 2 {
		t.Errorf("jobs after a second sweep = %d, want 2", got)
	}
}

func TestEnqueueDuePendingDoesNothingWhenDisabled(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: false, MaxBytes: attachments.DefaultMaxBytes})
	f.addAttachment(t, "m1", "https://cdn.example/1.png?ex=ffffffff", 10)

	n, err := d.EnqueueDuePending(f.ctx)
	if err != nil || n != 0 {
		t.Fatalf("EnqueueDuePending = %d, err %v; want 0 when disabled", n, err)
	}
	if got := f.countJobs(t, attachments.JobDownload); got != 0 {
		t.Errorf("jobs = %d, want 0", got)
	}
}

// A storage-side failure leaves its attachment pending on purpose, so
// without a cooldown the next sweep re-offers the whole batch a minute
// later and keeps doing it for as long as the disk stays full: hundreds
// of job rows and thousands of log lines a minute, none of which free any
// space. The sweep must pause instead, and say so exactly once.
func TestSweepPausesAfterStorageFailure(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fine content"))
	}))
	defer cdn.Close()

	f, d := newFixtureWithBlobs(t,
		attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes},
		failingBlobStore{err: errors.New("no space left on device")})
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff", 12)

	n, err := d.EnqueueDuePending(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("first sweep = %d, err %v; want 1", n, err)
	}
	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 10)); err == nil {
		t.Fatal("storage failure returned nil; it must stay retryable")
	}

	// Clear the queue so the dedupe key cannot be what suppresses the
	// next sweep: the cooldown has to be the reason.
	if _, err := f.pool().Exec(f.ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}

	n, err = d.EnqueueDuePending(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sweep after a storage failure scheduled %d, want 0 — a full disk must not spin the pipeline", n)
	}
	if got := f.countJobs(t, attachments.JobDownload); got != 0 {
		t.Errorf("jobs = %d, want 0", got)
	}
	if got := f.logLinesContaining("sweep paused"); got != 1 {
		t.Errorf("paused-sweep log lines = %d, want exactly 1 — one line per sweep, never one per attachment", got)
	}
	if got := f.attachmentStatus(t, id); got != "pending" {
		t.Errorf("status = %q, want pending", got)
	}
}

// The window between a first failure and a permanent verdict has to
// outlast a CDN wobble, not a single breath. With jobs.Backoff (3s, 6s,
// 12s ... capped at 10 min), ten attempts span about 23 minutes; five
// spanned 45 seconds.
func TestSweepGivesDownloadsAWideRetryWindow(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.addAttachment(t, "m1", "https://cdn.example/1.png?ex=ffffffff", 10)

	if _, err := d.EnqueueDuePending(f.ctx); err != nil {
		t.Fatal(err)
	}

	var maxAttempts int
	var window time.Duration
	if err := f.pool().QueryRow(f.ctx,
		`SELECT max_attempts FROM jobs WHERE kind = $1`, attachments.JobDownload).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < maxAttempts; attempt++ {
		window += jobs.Backoff(attempt)
	}
	if window < 15*time.Minute {
		t.Errorf("retry window = %s over %d attempts; want at least 15m before a transient failure becomes permanent",
			window, maxAttempts)
	}
}
