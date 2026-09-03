package attachments

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/storage"
	"github.com/openconvo/openconvo/internal/testutil"
)

// A blob that gains a reference after being listed must keep both its
// row and its bytes. Reclamation lists candidates in bulk, so this
// interleaving is ordinary, not exotic: reclaim is called here with a
// blob that already has a live reference by the time it runs, exactly
// as if a download had landed in the gap between listing and reclaiming.
func TestReclaimKeepsLateReference(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)
	store := archive.New(pool)
	blobs, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New(store, blobs, jobs.NewQueue(pool), nil, Options{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "g1", Name: "guild",
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID, ExternalID: "c1", Kind: "text", Name: "general",
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "here"
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ExternalID: "m1",
		Content: &content, SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := blobs.Put(ctx, bytes.NewReader([]byte("raced!")))
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := store.EnsureBlob(ctx, res.SHA256, res.Size, "text/plain", res.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	attID, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "f.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The race: a reference lands on the blob after it was already
	// picked as a reclamation candidate but before reclaim ran.
	if err := store.MarkAttachmentStored(ctx, attID, blobID); err != nil {
		t.Fatal(err)
	}

	ok, err := d.reclaim(ctx, archive.OrphanBlob{ID: blobID, SHA256: res.SHA256})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if ok {
		t.Error("reclaim reported removing a blob that is still referenced")
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE id = $1::uuid`, blobID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("blob rows = %d, want 1 (a referenced blob must survive)", rows)
	}
	if _, err := blobs.Open(ctx, res.SHA256); err != nil {
		t.Errorf("file was deleted despite a live reference: %v", err)
	}
}
