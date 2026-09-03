package discord_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/discord/discordtest"
	"github.com/openconvo/openconvo/internal/ingest"
	"github.com/openconvo/openconvo/internal/testutil"
)

func TestSourceRunAppliesGatewayEvents(t *testing.T) {
	pool := testutil.NewDB(t)
	store := archive.New(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingester := ingest.New(store, logger)

	s := discordtest.New(t)
	source := discord.NewSource("test-token")
	source.Client().WithBaseURL(s.BaseURL())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, discord.SourceDeps{Ingester: ingester, Logger: logger})
	}()
	t.Cleanup(func() { cancel(); <-done })

	s.WaitForSession(t)
	waitFor(t, func() bool {
		status := source.Status()
		return status.Connected && status.BotUsername == "openconvo" && status.LastError == ""
	})

	// Discovery via GUILD_CREATE, as real Discord sends after READY.
	s.Dispatch(t, "GUILD_CREATE", map[string]any{
		"id": "g1", "name": "FBFR",
		"channels": []map[string]any{
			{"id": "c1", "type": 0, "name": "deck-making"},
		},
		"threads": []map[string]any{
			{"id": "t1", "type": 11, "name": "old thread", "parent_id": "c1"},
		},
	})

	waitDB(t, func() bool {
		ch, ok, _ := store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "c1")
		return ok && ch.Name == "deck-making"
	})
	waitDB(t, func() bool {
		th, ok, _ := store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "t1")
		return ok && th.ParentChannelID != nil
	})

	// Enable the channel; live events must then be archived.
	ch, _, _ := store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "c1")
	if err := store.SetChannelArchiveEnabled(ctx, ch.ID, true); err != nil {
		t.Fatal(err)
	}
	ingester.InvalidateAllChannels()

	s.Dispatch(t, "MESSAGE_CREATE", map[string]any{
		"id": "1001", "channel_id": "c1", "content": "hello",
		"timestamp": "2026-08-19T10:00:00Z", "type": 0,
		"author": map[string]any{"id": "u1", "username": "john"},
	})
	waitDB(t, func() bool {
		m, ok, _ := store.GetMessageByExternalID(ctx, ch.ID, "1001")
		return ok && m.Content != nil && *m.Content == "hello"
	})

	s.Dispatch(t, "MESSAGE_UPDATE", map[string]any{
		"id": "1001", "channel_id": "c1", "content": "hello (edited)",
		"edited_timestamp": "2026-08-19T10:05:00Z",
	})
	waitDB(t, func() bool {
		m, _, _ := store.GetMessageByExternalID(ctx, ch.ID, "1001")
		return m.Content != nil && *m.Content == "hello (edited)"
	})

	s.Dispatch(t, "MESSAGE_REACTION_ADD", map[string]any{
		"channel_id": "c1", "message_id": "1001",
		"emoji": map[string]any{"id": nil, "name": "👍"},
	})
	waitDB(t, func() bool {
		m, _, _ := store.GetMessageByExternalID(ctx, ch.ID, "1001")
		rs, _ := store.ListReactions(ctx, m.ID)
		return len(rs) == 1 && rs[0].Count == 1
	})

	s.Dispatch(t, "MESSAGE_DELETE", map[string]any{"id": "1001", "channel_id": "c1"})
	waitDB(t, func() bool {
		m, _, _ := store.GetMessageByExternalID(ctx, ch.ID, "1001")
		return m.DeletedAt != nil && m.Content == nil
	})

	// A message for a channel that was never enabled is dropped.
	s.Dispatch(t, "GUILD_CREATE", map[string]any{
		"id": "g1", "name": "FBFR",
		"channels": []map[string]any{{"id": "c2", "type": 0, "name": "private-stuff"}},
	})
	waitDB(t, func() bool {
		_, ok, _ := store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "c2")
		return ok
	})
	s.Dispatch(t, "MESSAGE_CREATE", map[string]any{
		"id": "2001", "channel_id": "c2", "content": "secret",
		"timestamp": "2026-08-19T10:00:00Z",
		"author":    map[string]any{"id": "u1", "username": "john"},
	})
	// Give the pipeline a moment, then confirm nothing was written.
	time.Sleep(300 * time.Millisecond)
	c2, _, _ := store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "c2")
	if _, found, _ := store.GetMessageByExternalID(ctx, c2.ID, "2001"); found {
		t.Fatal("message archived for disabled channel")
	}

	// Source-side channel deletion preserves the archive.
	s.Dispatch(t, "CHANNEL_DELETE", map[string]any{"id": "c1"})
	waitDB(t, func() bool {
		got, _, _ := store.GetChannel(ctx, ch.ID)
		return got.IsArchived
	})
}

func waitDB(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
