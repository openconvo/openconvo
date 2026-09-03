package syncer_test

import (
	"context"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/jobs"
)

// TestEndToEndDiscordArchive drives the full milestone-2 flow against
// the fake Discord: discovery → selection → backfill → live sync →
// delete, with the real worker executing real jobs on a real database.
func TestEndToEndDiscordArchive(t *testing.T) {
	e := newEnv(t) // store, queue, ingester, fake Discord, syncer
	ctx := e.ctx

	// Nothing is archived until the operator selects the channel, so
	// start from the off position newEnv does not use.
	if err := e.store.SetChannelArchiveEnabled(ctx, e.channel.ID, false); err != nil {
		t.Fatal(err)
	}
	e.ingester.InvalidateAllChannels()

	// The fake guild has one channel with 150 messages of history.
	e.server.AddGuild("g1", "FBFR")
	e.server.SetMessages("c1", fakeMessages("c1", 150))

	// Start the real source (gateway) and a real worker.
	source := discord.NewSource("test-token")
	source.Client().WithBaseURL(e.server.BaseURL())

	runCtx, cancel := context.WithCancel(ctx)
	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- source.Run(runCtx, discord.SourceDeps{
			Ingester: e.ingester,
			OnResync: func() { e.sync.ReconcileAll(context.WithoutCancel(runCtx)) },
			Logger:   discardLogger(),
		})
	}()
	worker := jobs.NewWorker(e.queue, discardLogger())
	e.sync.RegisterHandlers(worker)
	workerDone := make(chan struct{})
	go func() { worker.Run(runCtx); close(workerDone) }()
	t.Cleanup(func() { cancel(); <-sourceDone; <-workerDone })

	e.server.WaitForSession(t)

	// 1. Discovery: GUILD_CREATE announces the channel.
	e.server.Dispatch(t, "GUILD_CREATE", map[string]any{
		"id": "g1", "name": "FBFR",
		"channels": []map[string]any{{"id": "c1", "type": 0, "name": "deck-making"}},
	})
	waitDBCond(t, func() bool {
		_, ok, _ := e.store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "c1")
		return ok
	})
	ch, _, _ := e.store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, "c1")

	// 2. Before selection, live events are dropped: an unselected
	//    channel's content never reaches the archive.
	e.server.Dispatch(t, "MESSAGE_CREATE", map[string]any{
		"id": "8000", "channel_id": "c1", "content": "not selected yet",
		"timestamp": "2026-08-19T11:00:00Z", "type": 0,
		"author": map[string]any{"id": "u3", "username": "sam"},
	})
	time.Sleep(300 * time.Millisecond)
	if _, found, _ := e.store.GetMessageByExternalID(ctx, ch.ID, "8000"); found {
		t.Fatal("message archived before the channel was selected")
	}

	// 3. Selection triggers backfill through the real queue and worker.
	if err := e.store.SetChannelArchiveEnabled(ctx, ch.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.sync.ChannelToggled(ctx, ch.ID, true); err != nil {
		t.Fatal(err)
	}
	waitDBCond(t, func() bool {
		state, _ := e.store.GetOrCreateSyncState(ctx, ch.ID)
		return state.BackfillComplete
	})
	counts, _ := e.store.Counts(ctx)
	if counts.Messages != 150 {
		t.Fatalf("backfilled messages = %d, want 150", counts.Messages)
	}

	// 4. Live sync: create, edit and delete arrive over the gateway.
	e.server.Dispatch(t, "MESSAGE_CREATE", map[string]any{
		"id": "9001", "channel_id": "c1", "content": "fresh",
		"timestamp": "2026-08-19T12:00:00Z", "type": 0,
		"author": map[string]any{"id": "u2", "username": "alex"},
	})
	waitDBCond(t, func() bool {
		m, ok, _ := e.store.GetMessageByExternalID(ctx, ch.ID, "9001")
		return ok && m.Content != nil && *m.Content == "fresh"
	})

	e.server.Dispatch(t, "MESSAGE_UPDATE", map[string]any{
		"id": "9001", "channel_id": "c1", "content": "fresh (edited)",
	})
	waitDBCond(t, func() bool {
		m, _, _ := e.store.GetMessageByExternalID(ctx, ch.ID, "9001")
		return m.Content != nil && *m.Content == "fresh (edited)"
	})

	e.server.Dispatch(t, "MESSAGE_DELETE", map[string]any{"id": "9001", "channel_id": "c1"})
	waitDBCond(t, func() bool {
		m, _, _ := e.store.GetMessageByExternalID(ctx, ch.ID, "9001")
		return m.DeletedAt != nil && m.Content == nil
	})

	// 5. The deletion is ledgered, so restoring a backup cannot
	//    resurrect it.
	var ledger int
	if err := e.pool.QueryRow(ctx,
		"SELECT count(*) FROM deletion_ledger WHERE external_id = '9001'").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != 1 {
		t.Errorf("ledger rows = %d, want 1", ledger)
	}

	// 6. The sync overview reports the channel as synced with its count.
	rows, err := e.store.SyncOverview(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("overview = %+v (%v)", rows, err)
	}
	if rows[0].Status != archive.SyncStatusSynced || rows[0].MessageCount != 150 {
		t.Errorf("overview row = %+v", rows[0])
	}
}
