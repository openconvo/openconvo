package archive_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/testutil"
)

// fixture creates a community + channel + actor to hang messages on.
func fixture(t *testing.T) (context.Context, *pgxpool.Pool, *archive.Store, archive.Channel, archive.Actor) {
	t.Helper()
	ctx := context.Background()
	pool := testutil.NewDB(t)
	store := archive.New(pool)

	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source:     archive.SourceDiscord,
		ExternalID: "guild-1",
		Name:       "Fingerboard France",
	})
	if err != nil {
		t.Fatalf("UpsertCommunity: %v", err)
	}
	channel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID,
		ExternalID:  "chan-1",
		Kind:        "text",
		Name:        "deck-making",
	})
	if err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	actor, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source:     archive.SourceDiscord,
		ExternalID: "user-1",
		Username:   "john",
	})
	if err != nil {
		t.Fatalf("UpsertActor: %v", err)
	}
	return ctx, pool, store, channel, actor
}

func strPtr(s string) *string { return &s }

func TestUpsertCommunityIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)
	store := archive.New(pool)

	first, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "g1", Name: "Before",
		RawPayload: json.RawMessage(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "g1", Name: "After",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("upsert created a new row: %s != %s", first.ID, second.ID)
	}
	if second.Name != "After" {
		t.Errorf("Name = %q, want %q", second.Name, "After")
	}
	// Empty raw payload on the second event must not erase the stored one.
	if string(second.RawPayload) != `{"a": 1}` && string(second.RawPayload) != `{"a":1}` {
		t.Errorf("RawPayload erased by empty update: %s", second.RawPayload)
	}
}

func TestMessageLifecycle(t *testing.T) {
	ctx, pool, store, channel, actor := fixture(t)

	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ActorID:         &actor.ID,
		ExternalID:      "m1",
		Content:         strPtr("what glue are you using?"),
		SourceCreatedAt: created,
		RawPayload:      json.RawMessage(`{"id":"m1","content":"what glue are you using?"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if msg.Content == nil || *msg.Content != "what glue are you using?" {
		t.Fatalf("content = %v", msg.Content)
	}

	// Duplicate event: no new row, nothing changes.
	dup, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ActorID:         &actor.ID,
		ExternalID:      "m1",
		Content:         strPtr("what glue are you using?"),
		SourceCreatedAt: created,
	})
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if dup.ID != msg.ID {
		t.Errorf("duplicate created new row: %s != %s", dup.ID, msg.ID)
	}

	// Edit.
	editedAt := created.Add(5 * time.Minute)
	edited, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ExternalID:      "m1",
		Content:         strPtr("what glue are you using? (edit: for veneer)"),
		SourceCreatedAt: created,
		SourceUpdatedAt: &editedAt,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Content == nil || !strings.Contains(*edited.Content, "veneer") {
		t.Errorf("edit not applied: %v", edited.Content)
	}
	if edited.SourceUpdatedAt == nil || !edited.SourceUpdatedAt.Equal(editedAt) {
		t.Errorf("SourceUpdatedAt = %v, want %v", edited.SourceUpdatedAt, editedAt)
	}
	if edited.ActorID == nil || *edited.ActorID != actor.ID {
		t.Errorf("edit without actor erased actor_id: %v", edited.ActorID)
	}

	// A REST snapshot fetched before the edit may be applied afterward. Its
	// missing/older edit timestamp must not regress the live Gateway version.
	staleEditedAt := editedAt.Add(-time.Minute)
	for name, stale := range map[string]archive.MessageUpsert{
		"original create": {
			ChannelID: channel.ID, ExternalID: "m1", Content: strPtr("stale original"),
			SourceCreatedAt: created, RawPayload: json.RawMessage(`{"content":"stale original"}`),
		},
		"older edit": {
			ChannelID: channel.ID, ExternalID: "m1", Content: strPtr("stale edit"),
			SourceCreatedAt: created, SourceUpdatedAt: &staleEditedAt,
			RawPayload: json.RawMessage(`{"content":"stale edit"}`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.UpsertMessage(ctx, stale)
			if err != nil {
				t.Fatal(err)
			}
			if got.Content == nil || *got.Content != "what glue are you using? (edit: for veneer)" {
				t.Errorf("stale snapshot regressed content: %v", got.Content)
			}
			if got.SourceUpdatedAt == nil || !got.SourceUpdatedAt.Equal(editedAt) {
				t.Errorf("stale snapshot regressed source_updated_at: %v", got.SourceUpdatedAt)
			}
			if strings.Contains(string(got.RawPayload), "stale") {
				t.Errorf("stale snapshot regressed raw_payload: %s", got.RawPayload)
			}
		})
	}

	// Partial update event with no content must not erase content.
	partial, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ExternalID:      "m1",
		Content:         nil,
		SourceCreatedAt: created,
	})
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	if partial.Content == nil || !strings.Contains(*partial.Content, "veneer") {
		t.Errorf("partial update erased content: %v", partial.Content)
	}

	// Attach dependent records, then delete the message.
	attID, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "veneer.jpg", SourceURL: "https://example.test/veneer.jpg",
	})
	if err != nil {
		t.Fatalf("attachment: %v", err)
	}
	if attID == "" {
		t.Fatal("empty attachment id")
	}
	if err := store.SetReaction(ctx, msg.ID, "👍", "👍", 3, nil); err != nil {
		t.Fatalf("reaction: %v", err)
	}

	found, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "m1", created)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !found {
		t.Fatal("delete reported message not found")
	}

	tomb, ok, err := store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if err != nil || !ok {
		t.Fatalf("get tombstone: %v ok=%v", err, ok)
	}
	if tomb.Content != nil {
		t.Errorf("deleted message still has content: %q", *tomb.Content)
	}
	if tomb.DeletedAt == nil {
		t.Error("deleted message has no deleted_at")
	}
	if string(tomb.RawPayload) != "{}" {
		t.Errorf("raw_payload not scrubbed: %s", tomb.RawPayload)
	}

	// Dependent records are gone.
	var attachments, reactions int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM attachments WHERE message_id = $1::uuid", msg.ID).Scan(&attachments); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM message_reactions WHERE message_id = $1::uuid", msg.ID).Scan(&reactions); err != nil {
		t.Fatal(err)
	}
	if attachments != 0 || reactions != 0 {
		t.Errorf("dependents not removed: %d attachments, %d reactions", attachments, reactions)
	}

	// Ledger entry exists.
	var ledger int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM deletion_ledger WHERE source=$1 AND object_type=$2 AND external_id=$3",
		archive.SourceDiscord, archive.ObjectTypeMessage, "m1").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != 1 {
		t.Errorf("deletion ledger entries = %d, want 1", ledger)
	}

	// A stale create/update event must not resurrect the message.
	resurrected, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ExternalID:      "m1",
		Content:         strPtr("what glue are you using?"),
		SourceCreatedAt: created,
		RawPayload:      json.RawMessage(`{"id":"m1"}`),
	})
	if err != nil {
		t.Fatalf("stale upsert after delete: %v", err)
	}
	if resurrected.Content != nil {
		t.Errorf("stale event resurrected deleted content: %q", *resurrected.Content)
	}
	if resurrected.DeletedAt == nil {
		t.Error("tombstone lost after stale upsert")
	}
	if string(resurrected.RawPayload) != "{}" {
		t.Errorf("stale event restored raw_payload: %s", resurrected.RawPayload)
	}
}

// A partial update event carries only the fields it changed, and the
// Discord normalizer falls back to kind "default" whenever the payload
// omits the message type. Neither may erase what an earlier complete
// event recorded.
func TestPartialUpdateKeepsKindAndRawPayload(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	created := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ActorID:         &actor.ID,
		ExternalID:      "m-partial",
		Kind:            "reply",
		Content:         strPtr("original text"),
		SourceCreatedAt: created,
		RawPayload:      json.RawMessage(`{"id":"m-partial","type":19,"content":"original text"}`),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:       channel.ID,
		ExternalID:      "m-partial",
		Kind:            "default",
		SourceCreatedAt: created,
		RawPayload:      json.RawMessage(`{"id":"m-partial","embeds":[{"url":"https://example.test/x"}]}`),
	})
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	if updated.Kind != "reply" {
		t.Errorf("Kind = %q, want reply: partial update reset it to the fallback", updated.Kind)
	}

	var payload map[string]any
	if err := json.Unmarshal(updated.RawPayload, &payload); err != nil {
		t.Fatalf("raw_payload: %v", err)
	}
	if payload["type"] != float64(19) {
		t.Errorf("raw_payload lost type: %s", updated.RawPayload)
	}
	if payload["content"] != "original text" {
		t.Errorf("raw_payload lost content: %s", updated.RawPayload)
	}
	if _, ok := payload["embeds"]; !ok {
		t.Errorf("raw_payload did not gain embeds: %s", updated.RawPayload)
	}
}

func TestSearchMessages(t *testing.T) {
	ctx, _, store, channel, john := fixture(t)
	otherChannel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: channel.CommunityID, ExternalID: "chan-2", Kind: "text", Name: "finishing",
	})
	if err != nil {
		t.Fatalf("second channel: %v", err)
	}
	alex, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source: archive.SourceDiscord, ExternalID: "user-2", Username: "alex", DisplayName: "Alex Smith",
	})
	if err != nil {
		t.Fatalf("second actor: %v", err)
	}
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	first, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &john.ID, ExternalID: "search-1",
		Content: strPtr("Try maple veneer with a thin layer of glue."), SourceCreatedAt: base,
	})
	if err != nil {
		t.Fatalf("first message: %v", err)
	}
	if _, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: first.ID, ExternalID: "search-att", Filename: "maple.jpg",
	}); err != nil {
		t.Fatalf("attachment: %v", err)
	}
	second, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: otherChannel.ID, ActorID: &alex.ID, ExternalID: "search-2",
		Content: strPtr("The maple veneer arrived flat and ready."), SourceCreatedAt: base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("second message: %v", err)
	}
	deleted, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &john.ID, ExternalID: "search-deleted",
		Content: strPtr("A deleted maple veneer secret."), SourceCreatedAt: base.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("deleted message: %v", err)
	}
	if found, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, deleted.ExternalID, deleted.SourceCreatedAt); err != nil || !found {
		t.Fatalf("delete search fixture: found=%v err=%v", found, err)
	}

	page, err := store.SearchMessages(ctx, archive.SearchParams{Query: `"maple veneer"`, Limit: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Results) != 1 || !page.HasMore || page.Results[0].MessageID != second.ID {
		t.Fatalf("first page = %+v", page)
	}
	if !strings.Contains(page.Results[0].Excerpt, "<mark>") {
		t.Errorf("excerpt is not highlighted: %q", page.Results[0].Excerpt)
	}
	if page.Results[0].ChannelName != "finishing" || page.Results[0].CommunityName != "Fingerboard France" {
		t.Errorf("result location = %+v", page.Results[0])
	}
	if page.Results[0].Actor == nil || page.Results[0].Actor.DisplayName != "Alex Smith" {
		t.Errorf("result actor = %+v", page.Results[0].Actor)
	}

	attachmentOnly := true
	before := base.Add(12 * time.Hour)
	filtered, err := store.SearchMessages(ctx, archive.SearchParams{
		Query: "maple veneer", Author: "JOHN", ChannelID: channel.ID,
		Before: &before, HasAttachment: &attachmentOnly,
	})
	if err != nil {
		t.Fatalf("filtered search: %v", err)
	}
	if len(filtered.Results) != 1 || filtered.Results[0].MessageID != first.ID || !filtered.Results[0].HasAttachment {
		t.Fatalf("filtered results = %+v", filtered.Results)
	}

	withoutAttachment := false
	after := base.Add(12 * time.Hour)
	filtered, err = store.SearchMessages(ctx, archive.SearchParams{
		Query: "maple veneer", After: &after, HasAttachment: &withoutAttachment,
	})
	if err != nil {
		t.Fatalf("negative attachment filter: %v", err)
	}
	if len(filtered.Results) != 1 || filtered.Results[0].MessageID != second.ID {
		t.Fatalf("negative attachment results = %+v", filtered.Results)
	}
}

// A negated query matches every message that lacks the term, including
// one archived with no content yet — a first sighting through a partial
// update leaves content NULL. Highlighting must survive that NULL.
func TestSearchMessagesNegatedQueryWithNullContent(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	base := time.Date(2026, 2, 4, 8, 0, 0, 0, time.UTC)

	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "neg-1",
		Content: strPtr("Maple veneer, cold pressed."), SourceCreatedAt: base,
	}); err != nil {
		t.Fatalf("message with content: %v", err)
	}
	contentless, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "neg-2",
		Content: nil, SourceCreatedAt: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("message without content: %v", err)
	}
	if contentless.Content != nil {
		t.Fatalf("fixture content = %q, want NULL", *contentless.Content)
	}

	page, err := store.SearchMessages(ctx, archive.SearchParams{Query: "-zebra"})
	if err != nil {
		t.Fatalf("negated search: %v", err)
	}
	if len(page.Results) != 2 {
		t.Fatalf("negated search returned %d results, want 2", len(page.Results))
	}
	for _, result := range page.Results {
		if result.MessageID == contentless.ID && result.Excerpt != "" {
			t.Errorf("excerpt for NULL content = %q, want empty", result.Excerpt)
		}
	}
}

func TestBookmarkLifecycle(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	message, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "curate-1",
		Content:         strPtr("Use Titebond III for the cold press."),
		SourceCreatedAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	created, wasCreated, err := store.CreateBookmark(ctx, archive.BookmarkUpsert{
		MessageID: message.ID, Title: "Glue recommendation", Description: "Tested with maple veneer",
		Tags: []string{"glue", "veneer"}, Collection: "Deck making",
	})
	if err != nil || !wasCreated {
		t.Fatalf("CreateBookmark = %+v, created %v, err %v", created, wasCreated, err)
	}
	if created.Content == nil || !strings.Contains(*created.Content, "Titebond") ||
		created.ChannelName != "deck-making" || created.CommunityName != "Fingerboard France" ||
		created.Actor == nil || created.Actor.Username != "john" {
		t.Errorf("created bookmark read model = %+v", created)
	}

	// A gateway retry or a second save in the UI must not overwrite details.
	duplicate, wasCreated, err := store.CreateBookmarkBySourceIdentity(ctx,
		archive.SourceDiscord, channel.ExternalID, message.ExternalID)
	if err != nil || wasCreated || duplicate.ID != created.ID || duplicate.Title != "Glue recommendation" {
		t.Fatalf("duplicate = %+v, created %v, err %v", duplicate, wasCreated, err)
	}

	updated, err := store.UpdateBookmark(ctx, created.ID, archive.BookmarkUpsert{
		Title: "Cold press glue", Description: "The durable conclusion", Tags: []string{"glue"}, Collection: "Pressing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Cold press glue" || updated.Collection != "Pressing" || len(updated.Tags) != 1 {
		t.Errorf("updated = %+v", updated)
	}
	rows, err := store.ListBookmarks(ctx, archive.BookmarkFilter{Collection: "Pressing", Tag: "glue"})
	if err != nil || len(rows) != 1 || rows[0].ID != created.ID {
		t.Fatalf("filtered list = %+v, err %v", rows, err)
	}
	rows, err = store.ListBookmarks(ctx, archive.BookmarkFilter{Tag: "veneer"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("stale tag filter = %+v, err %v", rows, err)
	}

	messageContext, found, err := store.GetMessageContext(ctx, message.ID, 0, 0)
	if err != nil || !found || messageContext.Messages[0].BookmarkID == nil || *messageContext.Messages[0].BookmarkID != created.ID {
		t.Fatalf("message bookmark marker = %+v, found %v, err %v", messageContext, found, err)
	}

	if err := store.DeleteBookmark(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListBookmarks(ctx, archive.BookmarkFilter{}); err != nil || len(rows) != 0 {
		t.Fatalf("after delete = %+v, err %v", rows, err)
	}
	if err := store.DeleteBookmark(ctx, created.ID); !errors.Is(err, archive.ErrNotFound) {
		t.Fatalf("second delete error = %v", err)
	}
}

func TestBookmarkCannotSaveOrSurviveDeletedMessage(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	message, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "deleted-curation",
		Content: strPtr("temporary"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	bookmark, _, err := store.CreateBookmark(ctx, archive.BookmarkUpsert{MessageID: message.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, message.ExternalID, message.SourceCreatedAt); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListBookmarks(ctx, archive.BookmarkFilter{}); err != nil || len(rows) != 0 {
		t.Fatalf("bookmark survived tombstone: %+v, err %v", rows, err)
	}
	if err := store.DeleteBookmark(ctx, bookmark.ID); !errors.Is(err, archive.ErrNotFound) {
		t.Fatalf("cascade did not delete bookmark: %v", err)
	}
	if _, _, err := store.CreateBookmark(ctx, archive.BookmarkUpsert{MessageID: message.ID}); !errors.Is(err, archive.ErrNotFound) {
		t.Fatalf("saved tombstone: %v", err)
	}
}

func TestGetStoredAttachment(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	created := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "with-file",
		Content: strPtr("the reference image"), SourceCreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	attachmentID, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "file-1", Filename: "carnival.webp",
		ContentType: "image/webp", Size: 1234, SourceURL: "https://example.test/carnival.webp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetStoredAttachment(ctx, attachmentID); err != nil || ok {
		t.Fatalf("pending GetStoredAttachment = ok %v, err %v", ok, err)
	}
	digest := strings.Repeat("5", 64)
	blobID, err := store.EnsureBlob(ctx, digest, 1200, "application/octet-stream", "sha256/55/"+digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, attachmentID, blobID); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.GetStoredAttachment(ctx, attachmentID)
	if err != nil || !ok {
		t.Fatalf("GetStoredAttachment = %+v, ok %v, err %v", got, ok, err)
	}
	if got.ID != attachmentID || got.Filename != "carnival.webp" || got.ContentType != "image/webp" || got.Size != 1200 || got.SHA256 != digest {
		t.Errorf("GetStoredAttachment = %+v", got)
	}

	// The timeline has to agree with the file the download link hands
	// over. Discord's declared size (1234 here) can differ from the bytes
	// its CDN served, so once a blob exists the blob's size is the honest
	// number to show beside the filename.
	page, err := store.ListMessages(ctx, channel.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var listed *archive.ArchiveAttachment
	for _, m := range page.Messages {
		for i := range m.Attachments {
			if m.Attachments[i].ID == attachmentID {
				listed = &m.Attachments[i]
			}
		}
	}
	if listed == nil {
		t.Fatal("stored attachment missing from the timeline")
	}
	if listed.Size != 1200 {
		t.Errorf("timeline size = %d, want the stored blob's 1200", listed.Size)
	}

	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "with-file", msg.SourceCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetStoredAttachment(ctx, attachmentID); err != nil || ok {
		t.Fatalf("deleted GetStoredAttachment = ok %v, err %v", ok, err)
	}
}

func TestDeleteUnknownMessageStillWritesLedger(t *testing.T) {
	ctx, pool, store, channel, _ := fixture(t)

	sourceCreatedAt := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	found, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "never-imported", sourceCreatedAt)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if found {
		t.Error("delete of unknown message reported found")
	}
	var ledger int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM deletion_ledger WHERE external_id = 'never-imported'").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != 1 {
		t.Errorf("ledger entries = %d, want 1", ledger)
	}

	content := "stale backfill content"
	blocked, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ExternalID: "never-imported", Content: &content,
		SourceCreatedAt: time.Now().UTC(), RawPayload: json.RawMessage(`{"content":"stale backfill content"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.Deleted() || blocked.Content != nil {
		t.Fatalf("deletion ledger did not block later insertion: %+v", blocked)
	}
	tombstone, found, err := store.GetMessageByExternalID(ctx, channel.ID, "never-imported")
	if err != nil || !found || !tombstone.Deleted() || tombstone.Content != nil {
		t.Fatalf("durable placeholder tombstone = %+v found=%v err=%v", tombstone, found, err)
	}
	if !tombstone.SourceCreatedAt.Equal(sourceCreatedAt) {
		t.Errorf("placeholder source_created_at = %v, want %v", tombstone.SourceCreatedAt, sourceCreatedAt)
	}
}

func TestConcurrentMessageInsertAndDeleteAlwaysEndsDeleted(t *testing.T) {
	ctx, _, store, channel, _ := fixture(t)
	for i := range 25 {
		externalID := fmt.Sprintf("race-%d", i)
		content := "must not survive"
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.UpsertMessage(ctx, archive.MessageUpsert{
				ChannelID: channel.ID, ExternalID: externalID, Content: &content,
				SourceCreatedAt: time.Now().UTC(),
			})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, externalID, time.Now().UTC())
			errs <- err
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}

		message, found, err := store.GetMessageByExternalID(ctx, channel.ID, externalID)
		if err != nil {
			t.Fatal(err)
		}
		if !found || !message.Deleted() || message.Content != nil {
			t.Fatalf("iteration %d ended live: %+v", i, message)
		}
	}
}

func TestReplyResolution(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// Reply arrives before its parent (normal during newest→oldest backfill).
	reply, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m2",
		Content: strPtr("Titebond III"), ReplyToExternalID: strPtr("m1"),
		SourceCreatedAt: base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.ReplyToMessageID != nil {
		t.Errorf("reply resolved to %v before parent exists", *reply.ReplyToMessageID)
	}
	if reply.ReplyToExternalID == nil || *reply.ReplyToExternalID != "m1" {
		t.Error("reply external reference not kept")
	}

	parent, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("what glue?"), SourceCreatedAt: base,
	})
	if err != nil {
		t.Fatalf("parent: %v", err)
	}

	// A second reply referencing an existing parent resolves immediately.
	second, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m3",
		Content: strPtr("same question!"), ReplyToExternalID: strPtr("m1"),
		SourceCreatedAt: base.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("second reply: %v", err)
	}
	if second.ReplyToMessageID == nil || *second.ReplyToMessageID != parent.ID {
		t.Errorf("reply not resolved: %v, want %s", second.ReplyToMessageID, parent.ID)
	}
}

func TestReactions(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("hello"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Live adds.
	for range 3 {
		if err := store.AdjustReaction(ctx, msg.ID, "👍", "👍", 1); err != nil {
			t.Fatalf("adjust +1: %v", err)
		}
	}
	rs, err := store.ListReactions(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Count != 3 {
		t.Fatalf("reactions = %+v, want one with count 3", rs)
	}

	// Live removes, floor at zero removes the row.
	for range 5 {
		if err := store.AdjustReaction(ctx, msg.ID, "👍", "", -1); err != nil {
			t.Fatalf("adjust -1: %v", err)
		}
	}
	rs, err = store.ListReactions(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Fatalf("reactions after removal = %+v, want none", rs)
	}

	// Absolute set (backfill) then remove-all.
	if err := store.SetReaction(ctx, msg.ID, "custom:123:kickflip", "kickflip", 7, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetReaction(ctx, msg.ID, "🔥", "🔥", 2, nil); err != nil {
		t.Fatal(err)
	}
	rs, _ = store.ListReactions(ctx, msg.ID)
	if len(rs) != 2 {
		t.Fatalf("reactions = %+v, want two", rs)
	}
	if err := store.RemoveAllReactions(ctx, msg.ID); err != nil {
		t.Fatal(err)
	}
	rs, _ = store.ListReactions(ctx, msg.ID)
	if len(rs) != 0 {
		t.Fatalf("reactions after remove-all = %+v, want none", rs)
	}
}

func TestBlobDeduplication(t *testing.T) {
	ctx, pool, store, channel, actor := fixture(t)
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("two copies of the same photo"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	const sha = "4ad390ab0000000000000000000000000000000000000000000000000000dead"
	blob1, err := store.EnsureBlob(ctx, sha, 1234, "image/jpeg", "sha256/4a/"+sha)
	if err != nil {
		t.Fatalf("EnsureBlob 1: %v", err)
	}
	blob2, err := store.EnsureBlob(ctx, sha, 1234, "image/jpeg", "sha256/4a/"+sha)
	if err != nil {
		t.Fatalf("EnsureBlob 2: %v", err)
	}
	if blob1 != blob2 {
		t.Errorf("same content produced two blobs: %s, %s", blob1, blob2)
	}

	att1, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "photo.jpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	att2, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a2", Filename: "photo-again.jpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, att1, blob1); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, att2, blob1); err != nil {
		t.Fatal(err)
	}

	var refs int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM attachments WHERE blob_id = $1::uuid AND download_status = 'stored'",
		blob1).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 2 {
		t.Errorf("blob references = %d, want 2", refs)
	}
}

func TestCounts(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	for i := range 3 {
		if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
			ChannelID: channel.ID, ActorID: &actor.ID,
			ExternalID: []string{"m1", "m2", "m3"}[i], Content: strPtr("hey"),
			SourceCreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "m3", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	counts, err := store.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Communities != 1 || counts.Channels != 1 || counts.Messages != 2 {
		t.Errorf("counts = %+v, want 1 community, 1 channel, 2 live messages", counts)
	}
}

func TestAttachmentStats(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("files"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	blobID, err := store.EnsureBlob(ctx, strings.Repeat("d", 64), 4096, "application/octet-stream", "sha256/dd/x")
	if err != nil {
		t.Fatal(err)
	}

	// Two attachments dedup onto the same blob: StoredBytes must count
	// the blob once, not once per reference, and Stored must still count
	// both attachments.
	stored1, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "kept.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored2, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a2", Filename: "kept-again.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, stored1, blobID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, stored2, blobID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a3", Filename: "waiting.bin",
	}); err != nil {
		t.Fatal(err)
	}

	for _, ext := range []string{"a4", "a5", "a6"} {
		failed, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
			MessageID: msg.ID, ExternalID: ext, Filename: "lost.bin",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkAttachmentFailed(ctx, failed, "gone"); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := store.AttachmentStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stored != 2 || stats.Pending != 1 || stats.Failed != 3 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.StoredBytes != 4096 {
		t.Errorf("StoredBytes = %d, want 4096", stats.StoredBytes)
	}
}

func TestSetChannelArchiveEnabled(t *testing.T) {
	ctx, _, store, channel, _ := fixture(t)

	if channel.ArchiveEnabled {
		t.Fatal("channels must default to archive_enabled=false")
	}
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	// Re-upserting the channel (sync) must not reset the operator's choice.
	updated, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: channel.CommunityID, ExternalID: channel.ExternalID,
		Kind: "text", Name: "deck-making-renamed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ArchiveEnabled {
		t.Error("channel sync reset archive_enabled")
	}
	if updated.Name != "deck-making-renamed" {
		t.Errorf("Name = %q, want renamed", updated.Name)
	}
}

func TestSyncStateLifecycle(t *testing.T) {
	ctx, _, store, channel, _ := fixture(t)

	state, err := store.GetOrCreateSyncState(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != archive.SyncStatusPending || state.BackfillComplete {
		t.Fatalf("fresh state = %+v", state)
	}
	// Idempotent.
	again, err := store.GetOrCreateSyncState(ctx, channel.ID)
	if err != nil || again.ID != state.ID {
		t.Fatalf("GetOrCreateSyncState not idempotent: %v %s vs %s", err, again.ID, state.ID)
	}

	if err := store.SetSyncStatus(ctx, channel.ID, archive.SyncStatusImporting); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNewestSynced(ctx, channel.ID, "500"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateBackfillCheckpoint(ctx, channel.ID, "301"); err != nil {
		t.Fatal(err)
	}
	state, _ = store.GetOrCreateSyncState(ctx, channel.ID)
	if state.Status != archive.SyncStatusImporting || state.StartedAt == nil {
		t.Errorf("importing state = %+v", state)
	}
	if state.OldestExternalID == nil || *state.OldestExternalID != "301" {
		t.Errorf("checkpoint = %v", state.OldestExternalID)
	}
	if state.NewestExternalID == nil || *state.NewestExternalID != "500" {
		t.Errorf("newest = %v", state.NewestExternalID)
	}

	if err := store.MarkBackfillComplete(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	state, _ = store.GetOrCreateSyncState(ctx, channel.ID)
	if !state.BackfillComplete || state.Status != archive.SyncStatusSynced ||
		state.CompletedAt == nil || state.LastSyncedAt == nil {
		t.Errorf("complete state = %+v", state)
	}

	if err := store.SetSyncError(ctx, channel.ID, "discord said no"); err != nil {
		t.Fatal(err)
	}
	state, _ = store.GetOrCreateSyncState(ctx, channel.ID)
	if state.Status != archive.SyncStatusError || state.LastError != "discord said no" {
		t.Errorf("error state = %+v", state)
	}

	if err := store.TouchSynced(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	state, _ = store.GetOrCreateSyncState(ctx, channel.ID)
	if state.Status != archive.SyncStatusSynced || state.LastError != "" {
		t.Errorf("touched state = %+v", state)
	}
}

func TestChannelListingsAndLookups(t *testing.T) {
	ctx, _, store, channel, _ := fixture(t)

	communities, err := store.ListCommunities(ctx)
	if err != nil || len(communities) != 1 {
		t.Fatalf("communities = %v, err %v", communities, err)
	}

	channels, err := store.ListChannels(ctx, channel.CommunityID)
	if err != nil || len(channels) != 1 || channels[0].ID != channel.ID {
		t.Fatalf("channels = %v, err %v", channels, err)
	}

	got, ok, err := store.GetChannel(ctx, channel.ID)
	if err != nil || !ok || got.ID != channel.ID {
		t.Fatalf("GetChannel: %v ok=%v", err, ok)
	}
	_, ok, err = store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, channel.ExternalID)
	if err != nil || !ok {
		t.Fatalf("by external: %v ok=%v", err, ok)
	}
	_, ok, _ = store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "nope")
	if ok {
		t.Error("found nonexistent channel")
	}

	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _, err = store.GetChannel(ctx, channel.ID)
	if err != nil || !got.ArchiveEnabled {
		t.Fatalf("archive_enabled = %v, err %v", got.ArchiveEnabled, err)
	}
}

func TestSyncOverviewIncludesThreads(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	thread, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: channel.CommunityID, ExternalID: "thread-1",
		ParentChannelID: &channel.ID, Kind: "thread", Name: "a thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetOrCreateSyncState(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetOrCreateSyncState(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("hello"), SourceCreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.SyncOverview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The enabled channel and its thread appear; each carries counts.
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	var found bool
	for _, r := range rows {
		if r.ChannelID == channel.ID {
			found = true
			if r.MessageCount != 1 || r.CommunityName == "" {
				t.Errorf("row = %+v", r)
			}
		}
	}
	if !found {
		t.Error("enabled channel missing from overview")
	}
}

func TestArchiveBrowsingTimelineAndContext(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	thread, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: channel.CommunityID, ExternalID: "thread-browse",
		ParentChannelID: &channel.ID, Kind: "thread", Name: "glue details",
	})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: channel.CommunityID, ExternalID: "never-selected", Kind: "text", Name: "empty",
	})
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	messages := make([]archive.Message, 0, 6)
	for i := 1; i <= 6; i++ {
		input := archive.MessageUpsert{
			ChannelID: channel.ID, ActorID: &actor.ID,
			ExternalID:      fmt.Sprintf("browse-%d", i),
			Content:         strPtr(fmt.Sprintf("message %d", i)),
			SourceCreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if i == 4 {
			input.ReplyToExternalID = strPtr("browse-2")
		}
		message, err := store.UpsertMessage(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	attachmentID, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: messages[3].ID, ExternalID: "browse-file", Filename: "glue-chart.pdf",
		Description: "comparison chart", ContentType: "application/pdf", Size: 2048,
		SourceURL: "https://source.invalid/private-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetReaction(ctx, messages[3].ID, "👍", "👍", 3, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: thread.ID, ActorID: &actor.ID, ExternalID: "thread-message",
		Content: strPtr("thread answer"), SourceCreatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}

	channels, err := store.ListArchiveChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gotChannel, gotThread bool
	for _, got := range channels {
		switch got.ID {
		case channel.ID:
			gotChannel = true
			if got.MessageCount != 6 || got.CommunityName == "" || got.LastMessageAt == nil {
				t.Errorf("channel summary = %+v", got)
			}
		case thread.ID:
			gotThread = true
			if got.ParentChannelID == nil || *got.ParentChannelID != channel.ID || got.ParentKind != "text" {
				t.Errorf("thread summary = %+v", got)
			}
		case empty.ID:
			t.Error("empty unselected discovery channel appeared in archive browser")
		}
	}
	if !gotChannel || !gotThread {
		t.Fatalf("archive channels = %+v", channels)
	}

	latest, err := store.ListMessages(ctx, channel.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !latest.HasOlder || len(latest.Messages) != 2 || latest.Messages[0].ExternalID != "browse-5" || latest.Messages[1].ExternalID != "browse-6" {
		t.Fatalf("latest page = %+v", latest)
	}
	older, err := store.ListMessages(ctx, channel.ID, latest.Messages[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !older.HasOlder || len(older.Messages) != 2 || older.Messages[0].ExternalID != "browse-3" || older.Messages[1].ExternalID != "browse-4" {
		t.Fatalf("older page = %+v", older)
	}
	gotMessage := older.Messages[1]
	if gotMessage.ReplyTo == nil || gotMessage.ReplyTo.ID != messages[1].ID || gotMessage.ReplyTo.Actor == nil {
		t.Errorf("reply preview = %+v", gotMessage.ReplyTo)
	}
	if len(gotMessage.Attachments) != 1 || gotMessage.Attachments[0].ID != attachmentID || gotMessage.Attachments[0].DownloadStatus != "pending" {
		t.Errorf("attachments = %+v", gotMessage.Attachments)
	}
	if len(gotMessage.Reactions) != 1 || gotMessage.Reactions[0].Count != 3 {
		t.Errorf("reactions = %+v", gotMessage.Reactions)
	}
	encoded, err := json.Marshal(gotMessage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-token") {
		t.Error("archive read model exposed attachment source URL")
	}

	context, ok, err := store.GetMessageContext(ctx, messages[3].ID, 2, 2)
	if err != nil || !ok {
		t.Fatalf("context: ok=%v err=%v", ok, err)
	}
	if context.Channel.ID != channel.ID || context.TargetID != messages[3].ID || len(context.Messages) != 5 {
		t.Fatalf("context = %+v", context)
	}
	for i, want := range []string{"browse-2", "browse-3", "browse-4", "browse-5", "browse-6"} {
		if context.Messages[i].ExternalID != want {
			t.Errorf("context[%d] = %s, want %s", i, context.Messages[i].ExternalID, want)
		}
	}

	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "browse-4", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GetMessageContext(ctx, messages[3].ID, 2, 2); err != nil || ok {
		t.Fatalf("deleted target context: ok=%v err=%v", ok, err)
	}
}

// A message can carry no text at all and still say something: Discord
// system events (a member joining) synthesize their text client-side, and
// a sticker replaces the message body outright. Both reach the reader
// through the read model, so both are asserted here — including through a
// reply preview, which otherwise has no way to tell "no text" apart from
// "not archived".
func TestArchiveMessageDescribesTextlessMessages(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	base := time.Date(2026, 7, 27, 18, 15, 0, 0, time.UTC)

	join, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "join-1",
		Kind: "member_join", SourceCreatedAt: base,
		RawPayload: json.RawMessage(`{"type":7,"content":""}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	sticker, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "sticker-1",
		Kind: "reply", ReplyToExternalID: strPtr("join-1"),
		SourceCreatedAt: base.Add(time.Second),
		RawPayload: json.RawMessage(
			`{"type":19,"content":"","sticker_items":[{"id":"749054660769218631","name":"Wave","format_type":3}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A payload whose sticker_items is absent or malformed must not break
	// the timeline read for every other message in the channel.
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "odd-1",
		Content: strPtr("plain"), ReplyToExternalID: strPtr("sticker-1"),
		SourceCreatedAt: base.Add(2 * time.Second),
		RawPayload:      json.RawMessage(`{"type":0,"sticker_items":"not-an-array"}`),
	}); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListMessages(ctx, channel.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(page.Messages))
	}
	gotJoin, gotSticker, gotOdd := page.Messages[0], page.Messages[1], page.Messages[2]

	if gotJoin.ID != join.ID || gotJoin.Kind != "member_join" {
		t.Errorf("join message = %+v, want kind member_join", gotJoin)
	}
	if len(gotJoin.Stickers) != 0 {
		t.Errorf("join stickers = %+v, want none", gotJoin.Stickers)
	}

	if gotSticker.ID != sticker.ID {
		t.Fatalf("sticker message = %+v", gotSticker)
	}
	if len(gotSticker.Stickers) != 1 {
		t.Fatalf("stickers = %+v, want 1", gotSticker.Stickers)
	}
	if gotSticker.Stickers[0].Name != "Wave" || gotSticker.Stickers[0].ID != "749054660769218631" {
		t.Errorf("sticker = %+v, want Wave/749054660769218631", gotSticker.Stickers[0])
	}
	// The reply preview must carry enough to describe a parent that has no
	// text; without the kind a reader can only claim it is unavailable.
	if gotSticker.ReplyTo == nil || gotSticker.ReplyTo.ID != join.ID {
		t.Fatalf("reply preview = %+v", gotSticker.ReplyTo)
	}
	if gotSticker.ReplyTo.Kind != "member_join" {
		t.Errorf("reply preview kind = %q, want member_join", gotSticker.ReplyTo.Kind)
	}

	if len(gotOdd.Stickers) != 0 {
		t.Errorf("malformed sticker_items produced %+v, want none", gotOdd.Stickers)
	}
	if gotOdd.ReplyTo == nil || len(gotOdd.ReplyTo.Stickers) != 1 || gotOdd.ReplyTo.Stickers[0].Name != "Wave" {
		t.Errorf("reply preview stickers = %+v, want Wave", gotOdd.ReplyTo)
	}
}

func TestResolveReplyLinks(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	base := time.Now().UTC()

	// Child first (backfill order), parent later, link unresolved.
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m2",
		Content: strPtr("re"), ReplyToExternalID: strPtr("m1"),
		SourceCreatedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	parent, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("hi"), SourceCreatedAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := store.ResolveReplyLinks(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("resolved = %d, want 1", n)
	}
	child, _, _ := store.GetMessageByExternalID(ctx, channel.ID, "m2")
	if child.ReplyToMessageID == nil || *child.ReplyToMessageID != parent.ID {
		t.Errorf("link not resolved: %v", child.ReplyToMessageID)
	}
}

func TestListArchivedExternalIDsSince(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	for i, ext := range []string{"m1", "m2", "m3"} {
		if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
			ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: ext,
			Content: strPtr("x"), SourceCreatedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "m3", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	ids, err := store.ListArchivedExternalIDsSince(ctx, channel.ID, base.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// m2 only: m1 is before the cutoff, m3 is deleted.
	if len(ids) != 1 || ids[0] != "m2" {
		t.Errorf("ids = %v", ids)
	}
}

func TestPendingAttachmentQueries(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	// The sweep only offers attachments in channels the operator has
	// enabled, so an enabled channel is the baseline for these queries.
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}

	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("with a file"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "photo.png",
		ContentType: "image/png", Size: 1234,
		SourceURL: "https://cdn.example/photo.png?ex=deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListPendingAttachments(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, err %v", pending, err)
	}
	if pending[0].ID != id || pending[0].Size != 1234 || pending[0].Filename != "photo.png" {
		t.Errorf("pending[0] = %+v", pending[0])
	}

	got, ok, err := store.GetAttachment(ctx, id)
	if err != nil || !ok || got.SourceURL != "https://cdn.example/photo.png?ex=deadbeef" {
		t.Fatalf("GetAttachment = %+v ok=%v err=%v", got, ok, err)
	}

	if err := store.SetAttachmentSourceURL(ctx, id, "https://cdn.example/photo.png?ex=fresh"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = store.GetAttachment(ctx, id)
	if got.SourceURL != "https://cdn.example/photo.png?ex=fresh" {
		t.Errorf("source_url = %q", got.SourceURL)
	}

	if err := store.MarkAttachmentFailed(ctx, id, "file is gone at source"); err != nil {
		t.Fatal(err)
	}
	pending, _ = store.ListPendingAttachments(ctx, 10)
	if len(pending) != 0 {
		t.Errorf("failed attachment still pending: %v", pending)
	}
}

// A tombstoned message's files must never be fetched: the message is
// already gone from the archive, and downloading its attachments would
// re-acquire exactly the content the deletion removed.
func TestPendingAttachmentsSkipDeletedMessages(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}

	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m2",
		Content: strPtr("doomed"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a2", Filename: "x.png",
		SourceURL: "https://cdn.example/x.png",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, "m2", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListPendingAttachments(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want none for a deleted message", pending)
	}
}

// Downloading follows the operator's current selection, like every other
// path that fetches from Discord. A channel switched off must stop
// pulling files out of Discord, even though the metadata archived while
// it was on is kept.
func TestPendingAttachmentsSkipDisabledChannels(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("with a file"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "photo.png",
		SourceURL: "https://cdn.example/photo.png?ex=deadbeef",
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListPendingAttachments(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending while enabled = %v, err %v; want 1", pending, err)
	}

	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, false); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingAttachments(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want none: a disabled channel must not keep downloading", pending)
	}
}

// Threads are never selected individually, so a thread's files follow its
// parent's setting — the same inheritance SyncOverview and ingest use.
func TestPendingAttachmentsInheritThreadParentEnablement(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	thread, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: channel.CommunityID, ExternalID: "thread-1",
		ParentChannelID: &channel.ID, Kind: "thread", Name: "a thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: thread.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("in a thread"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "photo.png",
		SourceURL: "https://cdn.example/photo.png?ex=deadbeef",
	}); err != nil {
		t.Fatal(err)
	}

	// The thread itself is never enabled; only its parent is.
	pending, err := store.ListPendingAttachments(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want none while the parent is disabled", pending)
	}

	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingAttachments(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, err %v; want 1: a thread inherits its parent", pending, err)
	}
}

// A file that fails and later succeeds must not keep its old reason.
func TestMarkAttachmentStoredClearsError(t *testing.T) {
	ctx, pool, store, channel, actor := fixture(t)

	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("retried"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "f.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentFailed(ctx, id, "temporarily gone"); err != nil {
		t.Fatal(err)
	}
	blobID, err := store.EnsureBlob(ctx, strings.Repeat("e", 64), 9, "text/plain", "sha256/ee/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, id, blobID); err != nil {
		t.Fatal(err)
	}

	var reason *string
	if err := pool.QueryRow(ctx,
		`SELECT download_error FROM attachments WHERE id = $1::uuid`, id).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		t.Errorf("download_error = %q after a successful store, want NULL", *reason)
	}
}

func TestOrphanBlobQueries(t *testing.T) {
	ctx, _, store, channel, actor := fixture(t)

	msg, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: "m1",
		Content: strPtr("with a file"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	attachmentID, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: "a1", Filename: "f.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	referenced, err := store.EnsureBlob(ctx, strings.Repeat("a", 64), 10, "application/octet-stream", "sha256/aa/"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAttachmentStored(ctx, attachmentID, referenced); err != nil {
		t.Fatal(err)
	}
	orphan, err := store.EnsureBlob(ctx, strings.Repeat("b", 64), 20, "application/octet-stream", "sha256/bb/"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}

	// The grace period keeps brand-new blobs: a download links its blob
	// moments after creating it, and must not have it taken away.
	fresh, err := store.ListOrphanBlobs(ctx, time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Errorf("orphans within the grace period = %v, want none", fresh)
	}

	orphans, err := store.ListOrphanBlobs(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ID != orphan {
		t.Fatalf("orphans = %+v, want only the unreferenced blob", orphans)
	}

	if err := store.DeleteBlob(ctx, orphan); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}
	// A blob that gained a reference since it was listed must survive.
	if err := store.DeleteBlob(ctx, referenced); !errors.Is(err, archive.ErrBlobReferenced) {
		t.Errorf("DeleteBlob(referenced) = %v, want ErrBlobReferenced", err)
	}
}

func TestBlobExistsBySHA(t *testing.T) {
	ctx, _, store, _, _ := fixture(t)

	sha := strings.Repeat("d", 64)
	if _, err := store.EnsureBlob(ctx, sha, 5, "text/plain", "sha256/dd/"+sha); err != nil {
		t.Fatal(err)
	}

	exists, err := store.BlobExistsBySHA(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("BlobExistsBySHA = false for a stored digest, want true")
	}

	exists, err = store.BlobExistsBySHA(ctx, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("BlobExistsBySHA = true for an unknown digest, want false")
	}
}
