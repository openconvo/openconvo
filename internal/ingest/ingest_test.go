package ingest_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/ingest"
	"github.com/openconvo/openconvo/internal/testutil"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func strPtr(s string) *string { return &s }

func normalizedMessage(channelExt, ext, content string) *discord.NormalizedMessage {
	return &discord.NormalizedMessage{
		ExternalID:        ext,
		ChannelExternalID: channelExt,
		Author: &discord.NormalizedActor{
			ExternalID: "user-1", Username: "john", DisplayName: "John",
		},
		Kind:      "default",
		Content:   strPtr(content),
		CreatedAt: time.Now().UTC(),
		Attachments: []discord.NormalizedAttachment{
			{ExternalID: "a1", Filename: "pic.jpg", ContentType: "image/jpeg", Size: 10, SourceURL: "https://cdn.test/pic.jpg"},
		},
		Reactions: []discord.NormalizedReaction{{EmojiKey: "👍", EmojiName: "👍", Count: 2}},
	}
}

func setup(t *testing.T) (context.Context, *archive.Store, *ingest.Ingester, archive.Channel) {
	t.Helper()
	ctx := context.Background()
	pool := testutil.NewDB(t)
	store := archive.New(pool)
	ing := ingest.New(store, discardLogger())

	if _, err := ing.ApplyGuild(ctx, &discord.NormalizedGuild{ExternalID: "guild-1", Name: "FBFR"}); err != nil {
		t.Fatal(err)
	}
	channel, err := ing.ApplyChannel(ctx, "guild-1", &discord.NormalizedChannel{
		ExternalID: "chan-1", Kind: "text", Name: "deck-making",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, ing, channel
}

func TestMessagesDroppedForDisabledChannels(t *testing.T) {
	ctx, store, ing, channel := setup(t)

	// Never enabled: the gate reads archive_enabled=false from the store.
	applied, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m1", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("message applied to a channel that was never enabled")
	}
	if _, found, _ := store.GetMessageByExternalID(ctx, channel.ID, "m1"); found {
		t.Fatal("message content written for disabled channel")
	}

	// Enabled, archived into, then switched off. This is the dangerous
	// direction: the answer comes from the channel cache, which only a
	// selection toggle corrects.
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")
	if applied, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m2", "while enabled")); err != nil || !applied {
		t.Fatalf("apply while enabled: %v applied=%v", err, applied)
	}
	deselectChannel(t, ctx, store, ing, channel.ID)

	applied, err = ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m3", "after opt-out"))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("message applied to a channel the operator switched off")
	}
	if _, found, _ := store.GetMessageByExternalID(ctx, channel.ID, "m3"); found {
		t.Fatal("message content written after the channel was switched off")
	}
	// Switching a channel off stops new writes; it never erases what was
	// already archived.
	if _, found, _ := store.GetMessageByExternalID(ctx, channel.ID, "m2"); !found {
		t.Error("already-archived message lost when the channel was switched off")
	}
}

// deselectChannel turns archiving off the way the operator does, including
// the cache invalidation the syncer's channel-toggle hook performs. Without
// it the gate would keep answering from a stale "enabled" cache entry.
func deselectChannel(t *testing.T, ctx context.Context, store *archive.Store, ing *ingest.Ingester, channelID string) {
	t.Helper()
	if err := store.SetChannelArchiveEnabled(ctx, channelID, false); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateAllChannels()
}

// archivedThenDeselected archives one message into an enabled channel and
// then switches the channel off. The archived message deliberately stays,
// so resolveMessage still finds it: only the enablement gate stops later
// events from writing to a channel the operator de-selected.
func archivedThenDeselected(t *testing.T) (context.Context, *archive.Store, *ingest.Ingester, archive.Channel, archive.Message) {
	t.Helper()
	ctx, store, ing, channel := setup(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")
	if applied, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m1", "archived while enabled")); err != nil || !applied {
		t.Fatalf("apply while enabled: %v applied=%v", err, applied)
	}
	msg, found, err := store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if err != nil || !found {
		t.Fatalf("get archived message: %v found=%v", err, found)
	}
	deselectChannel(t, ctx, store, ing, channel.ID)
	return ctx, store, ing, channel, msg
}

func TestMessageDeleteHonoredAfterChannelDisabled(t *testing.T) {
	ctx, store, ing, channel, _ := archivedThenDeselected(t)

	found, err := ing.ApplyMessageDelete(ctx, "chan-1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("delete of an archived message was ignored after channel disablement")
	}
	msg, _, _ := store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if msg.DeletedAt == nil || msg.Content != nil {
		t.Errorf("delete for a de-selected channel was not honored: %+v", msg)
	}

	// An unknown message from a disabled channel is still outside the privacy
	// selection boundary: do not retain even its external identity.
	found, err = ing.ApplyMessageDelete(ctx, "chan-1", "never-archived")
	if err != nil || found {
		t.Fatalf("unknown disabled-channel delete: found=%v err=%v", found, err)
	}
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateAllChannels()
	applied, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "never-archived", "now selected"))
	if err != nil || !applied {
		t.Fatalf("message after re-enable: applied=%v err=%v", applied, err)
	}
	msg, found, err = store.GetMessageByExternalID(ctx, channel.ID, "never-archived")
	if err != nil || !found || msg.Content == nil || *msg.Content != "now selected" {
		t.Errorf("unknown disabled-channel delete created a suppression record: %+v found=%v err=%v", msg, found, err)
	}
}

func TestReactionDeltaDroppedAfterChannelDisabled(t *testing.T) {
	ctx, store, ing, _, msg := archivedThenDeselected(t)

	ok, err := ing.ApplyReactionDelta(ctx, "chan-1", "m1", "🔥", "🔥", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("reaction applied to a channel the operator switched off")
	}
	reactions, err := store.ListReactions(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Only the 👍 the message was archived with while enabled.
	if len(reactions) != 1 || reactions[0].EmojiKey != "👍" {
		t.Errorf("reactions = %+v", reactions)
	}
}

func TestReactionClearDroppedAfterChannelDisabled(t *testing.T) {
	ctx, store, ing, _, msg := archivedThenDeselected(t)

	ok, err := ing.ApplyReactionClear(ctx, "chan-1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("reaction clear applied to a channel the operator switched off")
	}
	// A clear that slipped past the gate would delete archived rows.
	reactions, err := store.ListReactions(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reactions) != 1 {
		t.Errorf("reactions after clear for a de-selected channel = %+v", reactions)
	}
}

func TestMessageFlowForEnabledChannel(t *testing.T) {
	ctx, store, ing, channel := setup(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")

	applied, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m1", "what glue?"))
	if err != nil || !applied {
		t.Fatalf("apply: %v applied=%v", err, applied)
	}

	msg, found, err := store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if err != nil || !found {
		t.Fatalf("get message: %v found=%v", err, found)
	}
	if msg.Content == nil || *msg.Content != "what glue?" || msg.ActorID == nil {
		t.Errorf("message = %+v", msg)
	}
	reactions, _ := store.ListReactions(ctx, msg.ID)
	if len(reactions) != 1 || reactions[0].Count != 2 {
		t.Errorf("reactions = %+v", reactions)
	}

	// Edit through the same path.
	edit := normalizedMessage("chan-1", "m1", "what glue? (edited)")
	editedAt := time.Now().UTC()
	edit.EditedAt = &editedAt
	if _, err := ing.ApplyMessage(ctx, edit); err != nil {
		t.Fatal(err)
	}
	msg, _, _ = store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if msg.Content == nil || *msg.Content != "what glue? (edited)" {
		t.Errorf("edit not applied: %v", msg.Content)
	}

	// Delete.
	found2, err := ing.ApplyMessageDelete(ctx, "chan-1", "m1")
	if err != nil || !found2 {
		t.Fatalf("delete: %v found=%v", err, found2)
	}
	msg, _, _ = store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if msg.Content != nil || msg.DeletedAt == nil {
		t.Errorf("tombstone = %+v", msg)
	}

	// A stale create for a tombstoned message must not resurrect it.
	applied, err = ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m1", "back from the dead"))
	if err != nil || !applied {
		t.Fatalf("stale event: %v applied=%v", err, applied)
	}
	msg, _, _ = store.GetMessageByExternalID(ctx, channel.ID, "m1")
	if msg.Content != nil || msg.DeletedAt == nil {
		t.Errorf("tombstone resurrected: %+v", msg)
	}
}

func TestThreadInheritsParentEnablement(t *testing.T) {
	ctx, store, ing, channel := setup(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")

	if _, err := ing.ApplyChannel(ctx, "guild-1", &discord.NormalizedChannel{
		ExternalID: "thread-1", ParentExternalID: "chan-1", Kind: "thread",
		Name: "glue thread", IsThread: true,
	}); err != nil {
		t.Fatal(err)
	}

	applied, err := ing.ApplyMessage(ctx, normalizedMessage("thread-1", "tm1", "in thread"))
	if err != nil || !applied {
		t.Fatalf("thread message: %v applied=%v", err, applied)
	}

	// A thread under a DISABLED parent is dropped.
	if _, err := ing.ApplyChannel(ctx, "guild-1", &discord.NormalizedChannel{
		ExternalID: "chan-2", Kind: "text", Name: "off-limits",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ing.ApplyChannel(ctx, "guild-1", &discord.NormalizedChannel{
		ExternalID: "thread-2", ParentExternalID: "chan-2", Kind: "thread", IsThread: true,
	}); err != nil {
		t.Fatal(err)
	}
	applied, err = ing.ApplyMessage(ctx, normalizedMessage("thread-2", "tm2", "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("thread under disabled parent was archived")
	}
}

func TestReactionDeltasAndClear(t *testing.T) {
	ctx, store, ing, channel := setup(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")
	if _, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m1", "hi")); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, err := ing.ApplyReactionDelta(ctx, "chan-1", "m1", "🔥", "🔥", 1); err != nil {
			t.Fatal(err)
		}
	}
	msg, _, _ := store.GetMessageByExternalID(ctx, channel.ID, "m1")
	reactions, _ := store.ListReactions(ctx, msg.ID)
	// 👍 from the create plus 🔥 = 2 rows.
	if len(reactions) != 2 {
		t.Fatalf("reactions = %+v", reactions)
	}

	if _, err := ing.ApplyReactionClear(ctx, "chan-1", "m1"); err != nil {
		t.Fatal(err)
	}
	reactions, _ = store.ListReactions(ctx, msg.ID)
	if len(reactions) != 0 {
		t.Errorf("reactions after clear = %+v", reactions)
	}

	// A reaction for an unknown message is a no-op, not an error.
	ok, err := ing.ApplyReactionDelta(ctx, "chan-1", "ghost", "🔥", "🔥", 1)
	if err != nil || ok {
		t.Errorf("unknown message reaction: ok=%v err=%v", ok, err)
	}
}

func TestBulkDeleteTombstonesEveryMessage(t *testing.T) {
	ctx, store, ing, channel := setup(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")
	for _, id := range []string{"m1", "m2"} {
		if _, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", id, "x")); err != nil {
			t.Fatal(err)
		}
	}

	// "m3" was never archived: it must not count as deleted.
	deleted, err := ing.ApplyMessageDeleteBulk(ctx, "chan-1", []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	for _, id := range []string{"m1", "m2"} {
		msg, _, _ := store.GetMessageByExternalID(ctx, channel.ID, id)
		if msg.DeletedAt == nil {
			t.Errorf("%s not tombstoned", id)
		}
	}
}

func TestChannelDeletedAtSourcePreservesArchive(t *testing.T) {
	ctx, store, ing, channel := setup(t)
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ing.InvalidateChannelCache("chan-1")
	if _, err := ing.ApplyMessage(ctx, normalizedMessage("chan-1", "m1", "kept")); err != nil {
		t.Fatal(err)
	}

	if err := ing.ChannelDeletedAtSource(ctx, "chan-1"); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := store.GetChannel(ctx, channel.ID)
	if !ch.IsArchived {
		t.Error("channel not marked archived after source deletion")
	}
	// The archived content survives: an archive that is useful even when
	// a channel is removed is the whole point.
	if _, found, _ := store.GetMessageByExternalID(ctx, channel.ID, "m1"); !found {
		t.Error("archived message lost after source channel deletion")
	}
}
