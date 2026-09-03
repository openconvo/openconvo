package attachments

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
)

type downloadPayload struct {
	AttachmentID string `json:"attachment_id"`
}

// errTooLarge stops a copy once the size cap is passed. It travels out
// through storage.Store.Put's error wrapping, so an oversize file is
// abandoned mid-stream and never lands in the blob store.
var errTooLarge = errors.New("attachment exceeds the size limit")

// terminalError is a failure no retry can fix: the file is gone at
// source, or it is one OpenConvo will not store.
type terminalError struct{ reason string }

func (e *terminalError) Error() string { return e.reason }

func terminal(format string, args ...any) error {
	return &terminalError{reason: fmt.Sprintf(format, args...)}
}

// storageError is a failure on our side of the line — a full disk above
// all. It always retries and never marks an attachment failed, because
// the fix belongs to the operator, not to the file.
type storageError struct{ err error }

func (e *storageError) Error() string { return e.err.Error() }
func (e *storageError) Unwrap() error { return e.err }

// capReader fails once more than max bytes have been read. Reading
// exactly the cap succeeds: the limit is a ceiling, not a fencepost.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// At the limit. One probe tells a file that ends here from one
		// that goes on.
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		switch {
		case n > 0:
			return 0, errTooLarge
		case err != nil:
			return 0, err
		default:
			return 0, nil
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// HandleDownload fetches one attachment and stores it.
func (d *Downloader) HandleDownload(ctx context.Context, job *jobs.Job) error {
	if !d.enabled {
		// Downloading was switched off after this job was queued. Jobs
		// outlive restarts, so the handler enforces the gate too rather
		// than trusting whoever enqueued it.
		return nil
	}

	var payload downloadPayload
	if err := job.UnmarshalPayload(&payload); err != nil {
		return err
	}

	att, ok, err := d.store.GetAttachment(ctx, payload.AttachmentID)
	if err != nil {
		return err
	}
	if !ok {
		// The message was deleted while this job waited. Nothing to do,
		// and nothing to report: the archive is in the state it wants.
		return nil
	}

	downloadCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	err = d.download(downloadCtx, att)

	// Recording the verdict must survive whatever killed the download,
	// the deadline above included: a file that reliably takes longer than
	// downloadTimeout would otherwise never be able to write down why,
	// and would retry from scratch forever. Same reasoning as
	// jobs.Worker.execute's finishCtx.
	finishCtx := context.WithoutCancel(ctx)

	switch {
	case err == nil:
		return nil

	case isTerminal(err):
		// Concluded: record why and stop. Returning nil marks the job
		// succeeded, because reaching a verdict is the job's purpose.
		return d.markFailed(finishCtx, att.ID, err.Error())

	case isStorageFailure(err):
		// Never terminal, however many attempts are burned. The sweep
		// re-enqueues it once the operator frees space — after a
		// cooldown, so a full disk does not spin the pipeline hot.
		d.noteStorageFailure(time.Now())
		d.logger.Error("attachment storage failed; leaving the attachment pending",
			"attachment_id", att.ID, "error", err)
		return err

	case job.Attempts >= job.MaxAttempts:
		return d.markFailed(finishCtx, att.ID, err.Error())

	default:
		return err
	}
}

func isTerminal(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}

func isStorageFailure(err error) bool {
	var s *storageError
	return errors.As(err, &s)
}

func (d *Downloader) markFailed(ctx context.Context, attachmentID, reason string) error {
	d.logger.Warn("attachment download failed permanently",
		"attachment_id", attachmentID, "reason", reason)
	if err := d.store.MarkAttachmentFailed(ctx, attachmentID, reason); err != nil {
		if errors.Is(err, archive.ErrNotFound) {
			return nil // already stored, or the message went away
		}
		return err
	}
	return nil
}

func (d *Downloader) download(ctx context.Context, att archive.PendingAttachment) error {
	if att.Size > d.maxBytes {
		return terminal("declared size %d bytes is above the %d byte limit", att.Size, d.maxBytes)
	}

	downloadURL := att.SourceURL
	refreshed := false

	// A signed URL carries its own expiry, so a doomed request can be
	// skipped rather than made and failed.
	if expiry, ok := urlExpiry(downloadURL); !ok || !expiry.After(time.Now()) {
		fresh, err := d.refresh(ctx, att)
		if err != nil {
			return err
		}
		downloadURL, refreshed = fresh, true
	}

	resp, err := d.fetch(ctx, downloadURL)
	if err != nil {
		return err
	}

	// Rejected despite looking fresh: the signature may have been
	// invalidated early. One refresh, one retry, then it is gone.
	if isRejection(resp.StatusCode) && !refreshed {
		// Drain before closing so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		fresh, err := d.refresh(ctx, att)
		if err != nil {
			return err
		}
		if resp, err = d.fetch(ctx, fresh); err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	if isRejection(resp.StatusCode) {
		return terminal("file is no longer available at source (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", redactURL(att.SourceURL), resp.StatusCode)
	}
	if resp.ContentLength > d.maxBytes {
		return terminal("served size %d bytes is above the %d byte limit", resp.ContentLength, d.maxBytes)
	}

	res, err := d.blobs.Put(ctx, &capReader{r: resp.Body, left: d.maxBytes})
	switch {
	case errors.Is(err, errTooLarge):
		return terminal("file is above the %d byte limit", d.maxBytes)
	case err != nil:
		return &storageError{err: err}
	}

	// Checked against what this response promised, never against the size
	// in the message metadata. Discord's CDN re-encodes or rewrites the
	// container of some media on delivery — the response carries an
	// x-discord-transform-duration header when it does — which moves the
	// served size in either direction while still serving the whole file.
	// Believing the declared size threw away complete images and reported
	// them to the operator as gone from source.
	//
	// A body cut short already fails inside the copy above, because
	// net/http enforces Content-Length itself; this guards the layers it
	// cannot see, capReader and the storage driver.
	if resp.ContentLength >= 0 && res.Size != resp.ContentLength {
		return fmt.Errorf("size mismatch: stored %d bytes, response declared %d",
			res.Size, resp.ContentLength)
	}

	blobID, err := d.store.EnsureBlob(ctx, res.SHA256, res.Size, att.ContentType, res.ObjectKey)
	if err != nil {
		return err
	}
	if err := d.store.MarkAttachmentStored(ctx, att.ID, blobID); err != nil {
		return err
	}

	// Reclamation can delete a blob's file in the gap between Put and
	// this point: a download that deduplicates onto an old, currently
	// unreferenced blob is invisible to the reference check GC just
	// made. Verifying here turns that race into one extra retry.
	exists, err := d.blobs.Exists(ctx, res.SHA256)
	if err != nil {
		return &storageError{err: err}
	}
	if !exists {
		return fmt.Errorf("blob %s vanished after storing; downloading again", res.SHA256)
	}

	d.logger.Debug("attachment stored", "attachment_id", att.ID, "bytes", res.Size)
	return nil
}

// refresh exchanges an attachment's URL for a working one and persists
// it, so a retry does not have to refresh again.
func (d *Downloader) refresh(ctx context.Context, att archive.PendingAttachment) (string, error) {
	if d.refresher == nil {
		return "", terminal("no URL refresher configured")
	}
	refreshed, err := d.refresher.RefreshAttachmentURLs(ctx, []string{att.SourceURL})
	if err != nil {
		return "", fmt.Errorf("refresh URL for attachment %s: %w", att.ID, err)
	}
	fresh, ok := refreshed[att.SourceURL]
	if !ok && len(refreshed) == 1 {
		// The reply is keyed by the URL sent, and the pipeline never
		// batches, so a single answer to a single request is this
		// attachment's answer even if the echoed key is not byte-exact
		// (a normalized escape, a host case, an added parameter). Keying
		// strictly on the echo would otherwise fail a whole backlog
		// permanently the day Discord changes how it spells a URL back.
		for _, v := range refreshed {
			fresh, ok = v, true
		}
	}
	if !ok || fresh == "" {
		// Retryable, deliberately not terminal: an empty answer is far
		// more likely to be a bad day at Discord than proof the file is
		// gone, and a terminal verdict here is permanent from attempt
		// one. A file really gone at source still ends up failed, via
		// the 404 the CDN answers with.
		return "", fmt.Errorf("refresh URL for attachment %s: no URL returned", att.ID)
	}
	if err := d.store.SetAttachmentSourceURL(ctx, att.ID, fresh); err != nil {
		return "", err
	}
	return fresh, nil
}

// isRejection reports the statuses a dead or unauthorized signed URL
// answers with.
func isRejection(status int) bool {
	return status == http.StatusUnauthorized ||
		status == http.StatusForbidden ||
		status == http.StatusNotFound
}

func (d *Downloader) fetch(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", redactURL(rawURL), urlErrorCause(err))
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", redactURL(rawURL), urlErrorCause(err))
	}
	return resp, nil
}

// urlErrorCause unwraps a *url.Error to the failure inside it. net/url
// and net/http put the whole request URL in the error they return —
// query string and CDN signature included; only userinfo is stripped —
// so wrapping one verbatim would print, one colon after redactURL, the
// very signature redactURL just removed. Those strings outlive the
// request in the log, in jobs.last_error and in
// attachments.download_error, so only the cause travels on.
func urlErrorCause(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err
	}
	return err
}

// urlExpiry reads Discord's signed-URL expiry from the ex= parameter, a
// hex Unix timestamp. ok is false when the URL carries no expiry, which
// callers treat as "refresh before using".
func urlExpiry(rawURL string) (time.Time, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return time.Time{}, false
	}
	raw := parsed.Query().Get("ex")
	if raw == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(raw, 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// redactURL strips the query string, which carries the CDN signature.
// Logs should identify a file without handing out access to it.
func redactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.RawQuery = ""
	return parsed.String()
}
