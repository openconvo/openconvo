package syncer_test

import (
	"context"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/syncer"
)

func countJobs(t *testing.T, e *env, kind string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		"SELECT count(*) FROM jobs WHERE kind = $1 AND status IN ('pending','running')", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitDBCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestChannelToggledEnqueuesOnce(t *testing.T) {
	e := newEnv(t)

	if err := e.sync.ChannelToggled(e.ctx, e.channel.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.sync.ChannelToggled(e.ctx, e.channel.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, e, syncer.JobBackfill); n != 1 {
		t.Errorf("backfill jobs = %d, want 1 (dedupe)", n)
	}

	if err := e.sync.ChannelToggled(e.ctx, e.channel.ID, false); err != nil {
		t.Fatal(err)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.Status != archive.SyncStatusDisabled {
		t.Errorf("status = %s, want disabled", state.Status)
	}
}

func TestRunRecoversIncompleteBackfillsAtStartup(t *testing.T) {
	e := newEnv(t)
	// An enabled channel with no completed backfill and no pending job:
	// exactly what a crash mid-import leaves behind.
	if _, err := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.sync.Run(runCtx); close(done) }()
	waitDBCond(t, func() bool { return countJobs(t, e, syncer.JobBackfill) == 1 })
	cancel()
	<-done
}

func TestEnqueueDueReconciles(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 3))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	// Freshly synced, so not due.
	n, err := e.sync.EnqueueDueReconciles(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("due = %d, want 0 right after sync", n)
	}
	// Age the last sync beyond the threshold.
	if _, err := e.pool.Exec(e.ctx,
		"UPDATE sync_states SET last_synced_at = now() - interval '7 hours'"); err != nil {
		t.Fatal(err)
	}
	n, err = e.sync.EnqueueDueReconciles(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || countJobs(t, e, syncer.JobReconcile) != 1 {
		t.Errorf("due = %d, jobs = %d, want 1/1", n, countJobs(t, e, syncer.JobReconcile))
	}

	// ReconcileAll enqueues regardless of age, but dedupe keeps it to one.
	e.sync.ReconcileAll(e.ctx)
	if n := countJobs(t, e, syncer.JobReconcile); n != 1 {
		t.Errorf("jobs after ReconcileAll = %d, want still 1", n)
	}
}

func TestThreadDiscoveredAfterParentBackfillIsBackfilled(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 3))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	// Nothing is owed once the parent is fully imported.
	n, err := e.sync.EnqueueDueBackfills(e.ctx)
	if err != nil || n != 0 {
		t.Fatalf("due = %d, err %v; want 0 after a complete backfill", n, err)
	}

	// A thread surfaces later — THREAD_CREATE, or one that existed
	// unseen while OpenConvo was down. The Gateway never replays the
	// history it already holds, so it needs a backfill of its own.
	thread, err := e.ingester.ApplyChannel(e.ctx, "g1", &discord.NormalizedChannel{
		ExternalID: "t5", ParentExternalID: "c1", Kind: "thread", Name: "late thread", IsThread: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.server.SetMessages("t5", fakeMessages("t5", 4))

	n, err = e.sync.EnqueueDueBackfills(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || countJobs(t, e, syncer.JobBackfill) != 1 {
		t.Fatalf("due = %d, jobs = %d, want 1/1 for the newly seen thread",
			n, countJobs(t, e, syncer.JobBackfill))
	}

	// Running that job imports the history the gateway never delivered.
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, thread.ID)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.store.GetMessageByExternalID(e.ctx, thread.ID, "4"); !ok {
		t.Error("thread history not imported")
	}
}

func TestChannelToggledOnCompletedChannelDoesNotRebackfill(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 3))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	// Disabling and re-enabling an already imported channel must not
	// replay its whole history.
	if err := e.sync.ChannelToggled(e.ctx, e.channel.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.sync.ChannelToggled(e.ctx, e.channel.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := countJobs(t, e, syncer.JobBackfill); n != 0 {
		t.Errorf("backfill jobs = %d, want 0 for a complete channel", n)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.Status != archive.SyncStatusSynced {
		t.Errorf("status = %s, want synced", state.Status)
	}
}
