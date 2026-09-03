package attachments_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/attachments"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/storage"
)

// A blob nothing references is removed from the database and the disk.
func TestGCRemovesOrphanedBlob(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})

	res, err := f.blobs.Put(f.ctx, bytes.NewReader([]byte("nobody's file")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnsureBlob(f.ctx, res.SHA256, res.Size, "text/plain", res.ObjectKey); err != nil {
		t.Fatal(err)
	}
	f.ageBlobs(t)

	if err := d.HandleGC(f.ctx, &jobs.Job{Kind: attachments.JobGC, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("HandleGC: %v", err)
	}

	if n := countBlobs(t, f); n != 0 {
		t.Errorf("blob rows = %d, want 0", n)
	}
	if _, err := f.blobs.Open(f.ctx, res.SHA256); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open after GC = %v, want ErrNotFound", err)
	}
}

// Deduplication means one blob can back many attachments. A blob with
// any reference left must survive, file and all.
func TestGCKeepsReferencedBlob(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})

	id := f.addAttachment(t, "m1", "https://cdn.example/1.bin?ex=ffffffff", 5)
	res, err := f.blobs.Put(f.ctx, bytes.NewReader([]byte("kept!")))
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := f.store.EnsureBlob(f.ctx, res.SHA256, res.Size, "text/plain", res.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkAttachmentStored(f.ctx, id, blobID); err != nil {
		t.Fatal(err)
	}
	f.ageBlobs(t)

	if err := d.HandleGC(f.ctx, &jobs.Job{Kind: attachments.JobGC, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("HandleGC: %v", err)
	}

	if n := countBlobs(t, f); n != 1 {
		t.Errorf("blob rows = %d, want 1", n)
	}
	if _, err := f.blobs.Open(f.ctx, res.SHA256); err != nil {
		t.Errorf("referenced blob was deleted: %v", err)
	}
}

// Deleting a message strands its file. That is the whole reason this job
// exists.
func TestGCReclaimsAfterMessageDeletion(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})

	id := f.addAttachment(t, "m1", "https://cdn.example/1.bin?ex=ffffffff", 7)
	res, err := f.blobs.Put(f.ctx, bytes.NewReader([]byte("doomed!")))
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := f.store.EnsureBlob(f.ctx, res.SHA256, res.Size, "text/plain", res.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkAttachmentStored(f.ctx, id, blobID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.MarkMessageDeleted(f.ctx, archive.SourceDiscord, f.channel.ID, "m1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.ageBlobs(t)

	if err := d.HandleGC(f.ctx, &jobs.Job{Kind: attachments.JobGC, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("HandleGC: %v", err)
	}
	if _, err := f.blobs.Open(f.ctx, res.SHA256); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("deleted message's file survived: %v", err)
	}
}

// A row whose file is already gone is finished work, not an error. The
// row-first ordering never produces that state itself — a crash between
// its two deletions leaves the opposite, a file with no row — but a
// half-restored backup, a blob directory pruned by hand, or the narrow
// race gc.go documents between the digest re-check and the file delete
// all do. Reclamation wants the row gone and no bytes left behind; one
// of those is already true.
func TestGCToleratesMissingFile(t *testing.T) {
	f, d := newFixture(t, attachments.Options{Enabled: true, MaxBytes: attachments.DefaultMaxBytes})

	sha := "cc" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if _, err := f.store.EnsureBlob(f.ctx, sha, 3, "text/plain", "sha256/cc/"+sha); err != nil {
		t.Fatal(err)
	}
	f.ageBlobs(t)

	if err := d.HandleGC(f.ctx, &jobs.Job{Kind: attachments.JobGC, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("HandleGC: %v", err)
	}
	if n := countBlobs(t, f); n != 0 {
		t.Errorf("blob rows = %d, want 0", n)
	}
}

// Reclamation runs whatever OPENCONVO_ATTACHMENTS_ENABLED says: it
// enforces deletion, which is not the operator's option, and an operator
// who enabled downloads, archived files and switched them off again still
// needs deletions honored. Options{} is the disabled pipeline, and the
// assertion goes through HandleGC — the level where a mistaken "if
// !d.enabled { return nil }" would sit.
func TestGCReclaimsWhileDownloadsAreDisabled(t *testing.T) {
	f, d := newFixture(t, attachments.Options{})

	res, err := f.blobs.Put(f.ctx, bytes.NewReader([]byte("orphan with downloads off")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnsureBlob(f.ctx, res.SHA256, res.Size, "text/plain", res.ObjectKey); err != nil {
		t.Fatal(err)
	}
	f.ageBlobs(t)

	if err := d.HandleGC(f.ctx, &jobs.Job{Kind: attachments.JobGC, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("HandleGC: %v", err)
	}
	if n := countBlobs(t, f); n != 0 {
		t.Errorf("blob rows = %d, want 0 — reclamation must not be gated on downloading", n)
	}
	if _, err := f.blobs.Open(f.ctx, res.SHA256); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("file survived with downloads disabled: %v — reclamation enforces deletion, which is not optional", err)
	}
}
