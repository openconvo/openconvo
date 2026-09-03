package attachments_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/attachments"
	"github.com/openconvo/openconvo/internal/jobs"
)

// The whole pipeline: a pending attachment is swept onto the queue, a
// real worker claims it, and the bytes end up in blob storage.
func TestEndToEndAttachmentDownload(t *testing.T) {
	content := []byte("an attachment that survives Discord")
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
	// An expired URL, exactly like an archive that predates the pipeline.
	id := f.addAttachment(t, "m1", cdn.URL+"/file.bin?ex=1", int64(len(content)))

	n, err := d.EnqueueDuePending(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("EnqueueDuePending = %d, err %v", n, err)
	}

	worker := jobs.NewWorker(f.queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.RegisterHandlers(worker)
	runCtx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(runCtx) }()
	// Belt-and-braces alongside the explicit cancel/wait below: if a
	// helper's t.Fatal fires from inside the polling loop's own
	// condition, execution never reaches either, and only a t.Cleanup
	// still runs to stop the worker.
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.After(30 * time.Second)
	for {
		status := f.attachmentStatus(t, id)
		if status == "stored" {
			break
		}
		if status != "pending" {
			t.Fatalf("attachment reached terminal status %q instead of stored", status)
		}
		select {
		case <-deadline:
			t.Fatalf("attachment never reached stored (status %q)", status)
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	<-done

	var sha string
	if err := f.pool().QueryRow(f.ctx, `
		SELECT b.sha256 FROM attachments a JOIN blobs b ON b.id = a.blob_id
		WHERE a.id = $1::uuid`, id).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	r, err := f.blobs.Open(f.ctx, sha)
	if err != nil {
		t.Fatalf("blob missing from storage: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != string(content) {
		t.Errorf("stored content = %q, want %q", got, content)
	}
}
