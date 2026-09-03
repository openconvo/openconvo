package preservation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/storage"
	"github.com/openconvo/openconvo/internal/testutil"
)

type preservationFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	store      *archive.Store
	blobs      *storage.Filesystem
	message    archive.Message
	actor      archive.Actor
	attachment string
	blobDigest string
}

func newPreservationFixture(t *testing.T) preservationFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.NewDB(t)
	store := archive.New(pool)
	blobs, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "guild-1", Name: "Guild", RawPayload: json.RawMessage(`{"id":"guild-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID, ExternalID: "channel-1", Name: "archive", RawPayload: json.RawMessage(`{"id":"channel-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source: archive.SourceDiscord, ExternalID: "user-1", Username: "keeper", RawPayload: json.RawMessage(`{"id":"user-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "preserve this"
	message, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "message-1", Content: &content,
		SourceCreatedAt: time.Now().UTC(), RawPayload: json.RawMessage(`{"id":"message-1","content":"preserve this"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetReaction(ctx, message.ID, "👍", "👍", 2, nil); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: message.ID, ExternalID: "attachment-1", Filename: "notes.txt", ContentType: "text/plain", Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	put, err := blobs.Put(ctx, strings.NewReader("attachment"))
	if err != nil {
		t.Fatal(err)
	}
	blobID, err := store.EnsureBlob(ctx, put.SHA256, put.Size, "text/plain", put.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, attachment, blobID); err != nil {
		t.Fatal(err)
	}
	return preservationFixture{
		ctx: ctx, pool: pool, store: store, blobs: blobs, message: message,
		actor: actor, attachment: attachment, blobDigest: put.SHA256,
	}
}

func TestExportAndVerifyRoundTrip(t *testing.T) {
	fixture := newPreservationFixture(t)
	destination := filepath.Join(t.TempDir(), "export")
	manifest, err := Export(fixture.ctx, ExportOptions{
		Pool: fixture.pool, Blobs: fixture.blobs, Destination: destination, OpenConvoVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Counts.Messages != 1 || manifest.Counts.Attachments != 1 || manifest.Counts.Blobs != 1 {
		t.Fatalf("counts = %+v", manifest.Counts)
	}
	if _, err := VerifyExport(fixture.ctx, destination); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownExportAndVerifyRoundTrip(t *testing.T) {
	fixture := newPreservationFixture(t)
	destination := filepath.Join(t.TempDir(), "export")
	manifest, err := Export(fixture.ctx, ExportOptions{
		Pool: fixture.pool, Blobs: fixture.blobs, Destination: destination,
		OpenConvoVersion: "test", RenderMarkdown: true,
		Now: func() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Renderings) != 1 || manifest.Renderings[0] != "markdown" {
		t.Fatalf("renderings = %v", manifest.Renderings)
	}
	channelPath := filepath.Join(destination, filepath.FromSlash(markdownChannelPath(fixture.message.ChannelID)))
	body, err := os.ReadFile(channelPath)
	if err != nil {
		t.Fatal(err)
	}
	markdown := string(body)
	for _, want := range []string{
		"# #archive", "keeper", "preserve this", "notes\\.txt",
		fixture.blobDigest, "👍 ×2", "message-" + fixture.message.ID,
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("Markdown channel is missing %q:\n%s", want, markdown)
		}
	}
	if _, err := VerifyExport(fixture.ctx, destination); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(channelPath, append(body, []byte("tampered\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyExport(fixture.ctx, destination); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered Markdown verification error = %v", err)
	}
}

func TestVerifyLiveRepairsUntrackedAndTemporaryFiles(t *testing.T) {
	fixture := newPreservationFixture(t)
	untracked, err := fixture.blobs.Put(fixture.ctx, strings.NewReader("untracked"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	untrackedPath := filepath.Join(fixture.blobs.Root(), filepath.FromSlash(untracked.ObjectKey))
	if err := os.Chtimes(untrackedPath, old, old); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp(filepath.Join(fixture.blobs.Root(), "tmp"), "stale-")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmpPath, old, old); err != nil {
		t.Fatal(err)
	}

	report, err := VerifyLive(fixture.ctx, fixture.pool, fixture.blobs, false, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid() || report.Untracked != 1 || report.StaleTemporary != 1 || report.Removed != 0 {
		t.Fatalf("read-only report = %+v", report)
	}
	report, err = VerifyLive(fixture.ctx, fixture.pool, fixture.blobs, true, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid() || report.Removed != 2 {
		t.Fatalf("repair report = %+v", report)
	}
	if _, err := os.Stat(untrackedPath); !os.IsNotExist(err) {
		t.Fatalf("untracked object still exists: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temporary object still exists: %v", err)
	}
}

func TestReplayDeletionsAfterRestore(t *testing.T) {
	fixture := newPreservationFixture(t)
	if _, err := fixture.store.MarkMessageDeleted(fixture.ctx, archive.SourceDiscord, fixture.message.ChannelID, fixture.message.ExternalID, fixture.message.SourceCreatedAt); err != nil {
		t.Fatal(err)
	}
	// Preserve compatibility with actor-deletion records created before the v1
	// delete-user command was deferred. Replay still understands those exports
	// even though current builds do not produce new actor records.
	if _, err := fixture.pool.Exec(fixture.ctx, `
		UPDATE actors SET username='deleted-user', display_name='', avatar_url='', raw_payload='{}'::jsonb
		WHERE id=$1::uuid`, fixture.actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `
		INSERT INTO deletion_ledger (source, object_type, external_id)
		VALUES ($1, $2, $3)`, archive.SourceDiscord, archive.ObjectTypeActor, fixture.actor.ExternalID); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "export")
	if _, err := Export(fixture.ctx, ExportOptions{
		Pool: fixture.pool, Blobs: fixture.blobs, Destination: destination, OpenConvoVersion: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate restoring a database snapshot from before the deletion.
	if _, err := fixture.pool.Exec(fixture.ctx,
		`UPDATE actors SET username='keeper', raw_payload='{"id":"user-1"}'::jsonb WHERE id=$1::uuid`,
		fixture.actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx,
		`UPDATE messages SET content='resurrected', deleted_at=NULL, raw_payload='{"content":"resurrected"}'::jsonb WHERE id=$1::uuid`,
		fixture.message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.UpsertAttachment(fixture.ctx, archive.AttachmentUpsert{
		MessageID: fixture.message.ID, ExternalID: "restored-attachment", Filename: "private.txt",
	}); err != nil {
		t.Fatal(err)
	}

	report, err := ReplayDeletions(fixture.ctx, fixture.pool, destination)
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerEntries != 2 || report.MessagesTombstoned != 1 || report.ActorsScrubbed != 1 {
		t.Fatalf("replay report = %+v", report)
	}
	var content *string
	var deletedAt *time.Time
	if err := fixture.pool.QueryRow(fixture.ctx,
		`SELECT content, deleted_at FROM messages WHERE id=$1::uuid`, fixture.message.ID).Scan(&content, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if content != nil || deletedAt == nil {
		t.Fatalf("message was resurrected: content=%v deleted_at=%v", content, deletedAt)
	}
	var attachmentCount int
	if err := fixture.pool.QueryRow(fixture.ctx,
		`SELECT count(*) FROM attachments WHERE message_id=$1::uuid`, fixture.message.ID).Scan(&attachmentCount); err != nil {
		t.Fatal(err)
	}
	if attachmentCount != 0 {
		t.Fatalf("restored attachments = %d, want 0", attachmentCount)
	}
}

func TestExportPinsTimestampsToUTC(t *testing.T) {
	fixture := newPreservationFixture(t)
	// An installation whose PostgreSQL runs in a local time zone must still
	// produce the UTC timestamps docs/archive-format.md promises.
	config := fixture.pool.Config().Copy()
	config.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var zone string
	if err := pool.QueryRow(fixture.ctx, `SHOW TimeZone`).Scan(&zone); err != nil {
		t.Fatal(err)
	}
	if zone != "Asia/Kolkata" {
		t.Fatalf("session time zone = %q; the test must export from a non-UTC session", zone)
	}

	destination := filepath.Join(t.TempDir(), "export")
	if _, err := Export(fixture.ctx, ExportOptions{
		Pool: pool, Blobs: fixture.blobs, Destination: destination, OpenConvoVersion: "test",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(destination, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		SourceCreatedAt string `json:"source_created_at"`
		CreatedAt       string `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(body), "\n", 2)[0]), &record); err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{record.SourceCreatedAt, record.CreatedAt} {
		if !strings.HasSuffix(stamp, "+00:00") {
			t.Errorf("exported timestamp %q is not UTC", stamp)
		}
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Errorf("exported timestamp %q is not RFC 3339: %v", stamp, err)
		}
	}
}

func TestVerifyExportDetectsLostMarkdownChannel(t *testing.T) {
	fixture := newPreservationFixture(t)
	destination := filepath.Join(t.TempDir(), "export")
	manifest, err := Export(fixture.ctx, ExportOptions{
		Pool: fixture.pool, Blobs: fixture.blobs, Destination: destination,
		OpenConvoVersion: "test", RenderMarkdown: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Counts.MarkdownChannels != 1 {
		t.Fatalf("manifest markdown channels = %d, want 1", manifest.Counts.MarkdownChannels)
	}
	// Lose the rendering the way a truncated copy would: the channel file
	// and its checksum line disappear together, leaving only the index.
	name := markdownChannelPath(fixture.message.ChannelID)
	if err := os.Remove(filepath.Join(destination, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
	sums, err := readChecksums(filepath.Join(destination, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	delete(sums, name)
	if err := os.Remove(filepath.Join(destination, "checksums.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(filepath.Join(destination, "checksums.sha256"), sums); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyExport(fixture.ctx, destination); err == nil || !strings.Contains(err.Error(), "manifest counts") {
		t.Fatalf("lost Markdown channel error = %v", err)
	}
}
