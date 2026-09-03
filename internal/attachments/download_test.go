package attachments_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openconvo/openconvo/internal/attachments"
)

func TestDownloadStoresFile(t *testing.T) {
	content := []byte("the actual bytes of an attachment")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	// An expiry far in the future: no refresh should be needed.
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff", int64(len(content)))

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload: %v", err)
	}

	var status, sha string
	var size int64
	if err := f.pool().QueryRow(f.ctx, `
		SELECT a.download_status, b.sha256, b.size
		FROM attachments a JOIN blobs b ON b.id = a.blob_id
		WHERE a.id = $1::uuid`, id).Scan(&status, &sha, &size); err != nil {
		t.Fatalf("attachment not linked to a blob: %v", err)
	}
	if status != "stored" {
		t.Errorf("status = %q, want stored", status)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}

	r, err := f.blobs.Open(f.ctx, sha)
	if err != nil {
		t.Fatalf("blob not in storage: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != string(content) {
		t.Errorf("stored content = %q", got)
	}
	if f.refresher.calls != 0 {
		t.Errorf("refresher called %d times for a live URL", f.refresher.calls)
	}
}

// A job whose attachment vanished (its message was deleted while the job
// waited) is finished work, not an error to retry.
func TestDownloadIgnoresMissingAttachment(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	err := d.HandleDownload(f.ctx, downloadJob("00000000-0000-0000-0000-000000000000", 1, 5))
	if err != nil {
		t.Fatalf("HandleDownload = %v, want nil", err)
	}
}

// Jobs outlive restarts, so a job queued while downloading was enabled
// must not run after the operator switches it off.
func TestDownloadDoesNothingWhenDisabled(t *testing.T) {
	requested := 0
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested++
		_, _ = w.Write([]byte("must not be fetched"))
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: false, MaxBytes: attachments.DefaultMaxBytes})
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff", 19)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload = %v, want nil", err)
	}
	if requested != 0 {
		t.Errorf("CDN requests = %d, want 0", requested)
	}
	if got := f.attachmentStatus(t, id); got != "pending" {
		t.Errorf("status = %q, want pending — a disabled download must stay queueable", got)
	}
}

// An expired URL is refreshed before the file is fetched — no doomed
// request first.
func TestDownloadRefreshesExpiredURL(t *testing.T) {
	content := []byte("refreshed content")
	var paths []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Query().Get("hm") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(content)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.refresher.replacement = cdn.URL + "/file.bin?ex=ffffffff&hm=sig"
	// ex=1 is 1970: long expired.
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=1", int64(len(content)))

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload: %v", err)
	}
	if f.refresher.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", f.refresher.calls)
	}
	if len(paths) != 1 {
		t.Errorf("CDN requests = %d, want 1 (no doomed first attempt)", len(paths))
	}
	if f.attachmentStatus(t, id) != "stored" {
		t.Errorf("status = %q, want stored", f.attachmentStatus(t, id))
	}
	// The working URL is kept, so a retry does not refresh again.
	att, _, _ := f.store.GetAttachment(f.ctx, id)
	if att.SourceURL != f.refresher.replacement {
		t.Errorf("source_url = %q, want the refreshed URL", att.SourceURL)
	}
}

// A URL that looks fresh but is rejected gets exactly one refresh-and-retry.
func TestDownloadRetriesOnceAfterRejection(t *testing.T) {
	content := []byte("second time lucky")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hm") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(content)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.refresher.replacement = cdn.URL + "/file.bin?ex=ffffffff&hm=sig"
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff", int64(len(content)))

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload: %v", err)
	}
	if f.refresher.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", f.refresher.calls)
	}
	if f.attachmentStatus(t, id) != "stored" {
		t.Errorf("status = %q, want stored", f.attachmentStatus(t, id))
	}
}

// Gone at source: refreshed and still 404. No point retrying for a day.
func TestDownloadMarksGoneFileFailed(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.refresher.replacement = cdn.URL + "/file.bin?ex=ffffffff&hm=sig"
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff", 10)

	// Attempt 1 of 5: terminal failures do not wait for the last attempt.
	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload = %v, want nil (concluded, not retryable)", err)
	}
	if got := f.attachmentStatus(t, id); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	if reason := f.attachmentError(t, id); reason == "" {
		t.Error("download_error is empty; the operator needs a reason")
	}
}

// Declared size above the cap: never even requested.
func TestDownloadRejectsOversizeBeforeFetching(t *testing.T) {
	requested := 0
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested++
		_, _ = w.Write([]byte("should never be fetched"))
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: 100})
	id := f.addAttachment(t, "m1", cdn.URL+"/big.bin?ex=ffffffff", 1000)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload = %v, want nil", err)
	}
	if requested != 0 {
		t.Errorf("CDN requests = %d, want 0", requested)
	}
	if got := f.attachmentStatus(t, id); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
}

// A file that lies about its size is caught mid-stream, and nothing
// oversize ever reaches the blob store.
func TestDownloadRejectsOversizeMidStream(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Flush mid-body so the response goes out chunked with no
		// Content-Length. Without that, httptest computes the length
		// itself and the declared-size gate catches the overrun before
		// capReader ever sees a byte — which is precisely the path this
		// test exists to cover.
		_, _ = w.Write(make([]byte, 50))
		_ = http.NewResponseController(w).Flush()
		_, _ = w.Write(make([]byte, 450))
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: 100})
	id := f.addAttachment(t, "m1", cdn.URL+"/liar.bin?ex=ffffffff", 10)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload = %v, want nil", err)
	}
	if got := f.attachmentStatus(t, id); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	if n := countBlobs(t, f); n != 0 {
		t.Errorf("blobs = %d, want 0", n)
	}
}

// A file of exactly the cap is fine — the limit is a ceiling, not a
// fencepost error.
func TestDownloadAcceptsExactlyMaxBytes(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 100))
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: 100})
	id := f.addAttachment(t, "m1", cdn.URL+"/exact.bin?ex=ffffffff", 100)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload: %v", err)
	}
	if got := f.attachmentStatus(t, id); got != "stored" {
		t.Errorf("status = %q, want stored", got)
	}
}

// Discord's CDN does not promise to serve back the byte count its
// message metadata declared: it re-encodes or rewrites the container of
// some media on delivery (the response carries an
// x-discord-transform-duration header), which moves the size in either
// direction. The file that arrives is still the whole file, so the
// declared size cannot be an integrity check — believing it discarded
// complete images and reported them as gone from source.
func TestDownloadStoresFileWhoseServedSizeDiffersFromMetadata(t *testing.T) {
	content := make([]byte, 500)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	// Metadata says 150 bytes; the CDN serves the re-encoded 500.
	id := f.addAttachment(t, "m1", cdn.URL+"/photo.jpg?ex=ffffffff", 150)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err != nil {
		t.Fatalf("HandleDownload: %v", err)
	}
	if got := f.attachmentStatus(t, id); got != "stored" {
		t.Errorf("status = %q, want stored (reason: %q)", got, f.attachmentError(t, id))
	}

	var size int64
	if err := f.pool().QueryRow(f.ctx, `
		SELECT b.size FROM attachments a JOIN blobs b ON b.id = a.blob_id
		WHERE a.id = $1::uuid`, id).Scan(&size); err != nil {
		t.Fatalf("attachment not linked to a blob: %v", err)
	}
	// The blob records what was actually stored, not what was claimed.
	if size != int64(len(content)) {
		t.Errorf("blob size = %d, want %d", size, len(content))
	}
}

// What the response itself promised is still load-bearing: a transfer cut
// short must never be mistaken for a stored file.
func TestDownloadRejectsTruncatedTransfer(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "500")
		_, _ = w.Write(make([]byte, 100))
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	id := f.addAttachment(t, "m1", cdn.URL+"/cut.bin?ex=ffffffff", 500)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err == nil {
		t.Fatal("HandleDownload = nil; a truncated transfer must not be accepted")
	}
	if got := f.attachmentStatus(t, id); got == "stored" {
		t.Error("status = stored; a truncated transfer must not be stored")
	}
}

// Transient trouble retries, and only the final attempt gives up.
func TestDownloadRetriesTransientThenFails(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	id := f.addAttachment(t, "m1", cdn.URL+"/flaky.bin?ex=ffffffff", 10)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 5)); err == nil {
		t.Fatal("attempt 1 returned nil; a 500 must retry")
	}
	if got := f.attachmentStatus(t, id); got != "pending" {
		t.Errorf("status after attempt 1 = %q, want pending", got)
	}

	if err := d.HandleDownload(f.ctx, downloadJob(id, 5, 5)); err != nil {
		t.Fatalf("final attempt = %v, want nil (concluded)", err)
	}
	if got := f.attachmentStatus(t, id); got != "failed" {
		t.Errorf("status after final attempt = %q, want failed", got)
	}
}

// A full disk is the operator's problem, not the file's. However many
// attempts are burned, the attachment stays pending so the work resumes
// by itself once there is space.
func TestDownloadKeepsPendingOnStorageFailure(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fine content"))
	}))
	defer cdn.Close()

	f, d := newFixtureWithBlobs(t,
		attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes},
		failingBlobStore{err: errors.New("no space left on device")})
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff", 12)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 5, 5)); err == nil {
		t.Fatal("storage failure returned nil; it must stay retryable")
	}
	if got := f.attachmentStatus(t, id); got != "pending" {
		t.Errorf("status = %q, want pending — a full disk must not fail files permanently", got)
	}
}

// The refresh reply is keyed by the URL that was sent, and the pipeline
// never batches — so one answer to a one-URL request is this
// attachment's answer, whether or not Discord echoed the key byte for
// byte. Insisting on an exact echo would fail an operator's whole
// backlog permanently the day Discord changes how it spells a URL back.
func TestDownloadUsesSingleRefreshAnswerWhateverItsKey(t *testing.T) {
	content := []byte("keyed under a normalized URL")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hm") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(content)
	}))
	defer cdn.Close()

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.refresher.replacement = cdn.URL + "/file.bin?ex=ffffffff&hm=sig"
	f.refresher.keyAs = "https://cdn.discordapp.com/attachments/1/2/file%20name.bin?ex=1"
	// Expired, so the download must refresh before fetching.
	id := f.addAttachment(t, "m1", cdn.URL+"/file%20name.bin?ex=1", int64(len(content)))

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 10)); err != nil {
		t.Fatalf("HandleDownload: %v", err)
	}
	if got := f.attachmentStatus(t, id); got != "stored" {
		t.Errorf("status = %q, want stored — a differently-keyed single answer is still the answer", got)
	}
}

// An empty refresh reply is Discord having a bad day far more often than
// it is proof the file is gone, and a terminal verdict here would be
// permanent from attempt one. It must retry instead.
func TestDownloadRetriesEmptyRefresh(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	f.refresher.empty = true
	id := f.addAttachment(t, "m1", "https://cdn.example/file.bin?ex=1", 10)

	if err := d.HandleDownload(f.ctx, downloadJob(id, 1, 10)); err == nil {
		t.Fatal("HandleDownload = nil; an empty refresh must retry")
	}
	if got := f.attachmentStatus(t, id); got != "pending" {
		t.Errorf("status = %q, want pending on attempt 1 of 10", got)
	}
}

// A signed CDN URL's query string is its access token: whoever holds it
// can read the file until ex= elapses, no authentication asked. Download
// failures are written down in places that outlive the request by a long
// way — the operator's logs, jobs.last_error, and the
// attachments.download_error column docs/self-hosting.md tells operators
// to inspect — so no signature may survive into one. The hard case is a
// transport failure: net/http answers with a *url.Error that repeats the
// whole URL, query string included, so wrapping it after a redacted
// prefix would hand the signature straight back.
func TestDownloadErrorsNeverCarryTheURLSignature(t *testing.T) {
	// Closed before anything is fetched, so d.http.Do fails with the
	// *url.Error this test exists for.
	cdn := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cdn.Close()

	// The three parameters Discord signs a CDN URL with. ex= is far in
	// the future, so the download goes straight to the fetch rather than
	// refreshing first.
	const signature = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"
	secrets := []string{"ex=ffffffff", "is=68a6b1c2", "hm=" + signature, signature}

	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=ffffffff&is=68a6b1c2&hm="+signature, 10)

	err := d.HandleDownload(f.ctx, downloadJob(id, 1, 10))
	if err == nil {
		t.Fatal("HandleDownload = nil; an unreachable CDN must retry")
	}
	// This string is what the worker logs and persists to jobs.last_error.
	assertRedacted(t, "returned error", err.Error(), secrets)

	// The last attempt writes the same verdict to the attachment.
	if err := d.HandleDownload(f.ctx, downloadJob(id, 10, 10)); err != nil {
		t.Fatalf("final attempt = %v, want nil (concluded)", err)
	}
	reason := f.attachmentError(t, id)
	if reason == "" {
		t.Fatal("download_error is empty; there is nothing to assert on")
	}
	assertRedacted(t, "download_error", reason, secrets)
	assertRedacted(t, "logs", f.logs.String(), secrets)
}

// assertRedacted fails if any secret survived into got, printing the
// offending text so a regression is diagnosable from the failure alone.
func assertRedacted(t *testing.T, where, got string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Errorf("%s leaks %q from the signed URL: %s", where, secret, got)
		}
	}
}
