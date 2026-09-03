package syncer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/discord/discordtest"
	"github.com/openconvo/openconvo/internal/ingest"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/syncer"
	"github.com/openconvo/openconvo/internal/testutil"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeMessages builds n raw messages with ids n..1 (newest first).
func fakeMessages(channelExt string, n int) []json.RawMessage {
	out := make([]json.RawMessage, 0, n)
	for i := n; i >= 1; i-- {
		out = append(out, json.RawMessage(fmt.Sprintf(
			`{"id":"%d","channel_id":%q,"content":"msg %d","timestamp":"2026-01-01T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`,
			i, channelExt, i)))
	}
	return out
}

type env struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	store    *archive.Store
	queue    *jobs.Queue
	ingester *ingest.Ingester
	server   *discordtest.Server
	sync     *syncer.Syncer
	channel  archive.Channel
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := testutil.NewDB(t)
	store := archive.New(pool)
	queue := jobs.NewQueue(pool)
	ingester := ingest.New(store, discardLogger())
	server := discordtest.New(t)
	client := discord.NewClient("t").WithBaseURL(server.BaseURL())
	sync := syncer.New(store, queue, client, ingester, discardLogger())

	if _, err := ingester.ApplyGuild(ctx, &discord.NormalizedGuild{ExternalID: "g1", Name: "FBFR"}); err != nil {
		t.Fatal(err)
	}
	channel, err := ingester.ApplyChannel(ctx, "g1", &discord.NormalizedChannel{
		ExternalID: "c1", Kind: "text", Name: "deck-making",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	ingester.InvalidateAllChannels()
	return &env{ctx: ctx, pool: pool, store: store, queue: queue, ingester: ingester, server: server, sync: sync, channel: channel}
}

func backfillJob(t *testing.T, channelID string) *jobs.Job {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"channel_id": channelID})
	return &jobs.Job{ID: "test-job", Kind: syncer.JobBackfill, Payload: payload, MaxAttempts: 10}
}

func TestBackfillCompletesAndCheckpoints(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 250))

	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatalf("HandleBackfill: %v", err)
	}

	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 250 {
		t.Errorf("messages = %d, want 250", counts.Messages)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if !state.BackfillComplete || state.Status != archive.SyncStatusSynced {
		t.Errorf("state = %+v", state)
	}
	if state.OldestExternalID == nil || *state.OldestExternalID != "1" {
		t.Errorf("oldest = %v", state.OldestExternalID)
	}
	if state.NewestExternalID == nil || *state.NewestExternalID != "250" {
		t.Errorf("newest = %v", state.NewestExternalID)
	}
	// 250 messages = pages of 100 + 100 + 50.
	if got := e.server.RequestCount("/channels/c1/messages"); got != 3 {
		t.Errorf("message requests = %d, want 3", got)
	}
}

func TestBackfillResumesFromCheckpointAfterFailure(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 250))
	// Page 1 succeeds, page 2 fails: the job must surface a retryable error.
	e.server.FailNextMessagesAfter(1)

	err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID))
	if err == nil {
		t.Fatal("expected transient failure to surface as job error")
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.BackfillComplete {
		t.Fatal("backfill marked complete despite failure")
	}
	if state.OldestExternalID == nil || *state.OldestExternalID != "151" {
		t.Fatalf("checkpoint = %v, want 151 after the first page", state.OldestExternalID)
	}

	// Retry, as the worker would: resume from the checkpoint without
	// re-fetching page 1.
	before := e.server.RequestCount("/channels/c1/messages")
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 250 {
		t.Errorf("messages = %d, want 250 with no duplicates", counts.Messages)
	}
	if got := e.server.RequestCount("/channels/c1/messages") - before; got != 2 {
		t.Errorf("retry made %d message requests, want 2 (pages 2 and 3)", got)
	}
}

func TestBackfillDoesNotCheckpointPastUnparseableMessage(t *testing.T) {
	e := newEnv(t)
	messages := fakeMessages("c1", 250)
	// Message 130 is in page two. Page one may be committed, but the page
	// containing an unsupported payload must remain retryable in full.
	messages[120] = json.RawMessage(`{"id":"130","channel_id":"c1","content":"bad timestamp","timestamp":"01/01/2026 00:00:00","type":0,"author":{"id":"u1","username":"john"}}`)
	e.server.SetMessages("c1", messages)

	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err == nil {
		t.Fatal("unparseable message did not fail the backfill")
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.BackfillComplete {
		t.Fatal("backfill marked complete after an unparseable message")
	}
	if state.OldestExternalID == nil || *state.OldestExternalID != "151" {
		t.Fatalf("checkpoint = %v, want last complete page at 151", state.OldestExternalID)
	}
}

func TestBackfillStopsWithoutCheckpointWhenChannelIsDisabledMidPage(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 250))
	responses := 0
	e.server.SetMessageResponseHook(func() {
		responses++
		if responses != 2 {
			return
		}
		if err := e.store.SetChannelArchiveEnabled(e.ctx, e.channel.ID, false); err != nil {
			t.Errorf("disable channel: %v", err)
		}
		e.ingester.InvalidateAllChannels()
	})

	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.BackfillComplete {
		t.Fatal("backfill marked complete after channel disablement")
	}
	if state.Status != archive.SyncStatusDisabled {
		t.Fatalf("status = %s, want disabled", state.Status)
	}
	if state.OldestExternalID == nil || *state.OldestExternalID != "151" {
		t.Fatalf("checkpoint = %v, want last enabled page at 151", state.OldestExternalID)
	}
}

func TestBackfillForbiddenMarksErrorWithoutRetry(t *testing.T) {
	e := newEnv(t)
	e.server.ForbidChannel("c1")

	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatalf("403 must not surface as a retryable error, got %v", err)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.Status != archive.SyncStatusError || state.LastError == "" {
		t.Errorf("state = %+v", state)
	}
	if state.BackfillComplete {
		t.Error("channel marked complete despite never being read")
	}
}

func TestBackfillEnqueuesThreadBackfills(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 10))
	e.server.AddArchivedThread("c1", json.RawMessage(
		`{"id":"t9","guild_id":"g1","type":11,"name":"old advice","parent_id":"c1","thread_metadata":{"archived":true,"archive_timestamp":"2026-05-01T10:00:00+00:00"}}`))
	e.server.SetMessages("t9", fakeMessages("t9", 3))

	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}

	// The thread was discovered and stored...
	thread, ok, _ := e.store.GetChannelBySourceExternalID(e.ctx, archive.SourceDiscord, "t9")
	if !ok {
		t.Fatal("archived thread not stored")
	}
	// ...and a backfill job was enqueued for it.
	var enqueued int
	if err := e.pool.QueryRow(e.ctx,
		"SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'channel_id' = $2",
		syncer.JobBackfill, thread.ID).Scan(&enqueued); err != nil {
		t.Fatal(err)
	}
	if enqueued != 1 {
		t.Fatalf("thread backfill jobs = %d, want 1", enqueued)
	}

	// Running the thread's job archives its messages through the
	// enablement it inherits from the parent.
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, thread.ID)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.store.GetMessageByExternalID(e.ctx, thread.ID, "3"); !ok {
		t.Error("thread message not archived")
	}
	// A thread has no threads of its own: no pointless REST call.
	if got := e.server.RequestCount("/channels/t9/threads"); got != 0 {
		t.Errorf("thread fan-out ran for a thread: %d requests", got)
	}
}

func TestBackfillSkipsDisabledChannel(t *testing.T) {
	e := newEnv(t)
	if err := e.store.SetChannelArchiveEnabled(e.ctx, e.channel.ID, false); err != nil {
		t.Fatal(err)
	}
	e.ingester.InvalidateAllChannels()
	e.server.SetMessages("c1", fakeMessages("c1", 5))

	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 0 {
		t.Errorf("messages archived for disabled channel: %d", counts.Messages)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.Status != archive.SyncStatusDisabled {
		t.Errorf("status = %s, want disabled", state.Status)
	}
	// No REST traffic at all for a disabled channel.
	if got := e.server.RequestCount("/channels/c1/messages"); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

func TestBackfillForUnknownChannelIsNoOp(t *testing.T) {
	e := newEnv(t)
	// A channel deleted between enqueue and execution must not fail the
	// job forever.
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, "0198c0de-0000-4000-8000-000000000001")); err != nil {
		t.Errorf("unknown channel: %v", err)
	}
}
