package syncer_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/syncer"
)

func reconcileJob(t *testing.T, channelID string) *jobs.Job {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"channel_id": channelID})
	return &jobs.Job{ID: "test-reconcile", Kind: syncer.JobReconcile, Payload: payload, MaxAttempts: 10}
}

func TestReconcileFillsGapsEditsAndDeletions(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 5))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 5 {
		t.Fatalf("setup: %d messages", counts.Messages)
	}

	// Meanwhile on "Discord": message 3 deleted, message 2 edited,
	// messages 6 and 7 posted — and the gateway missed all of it.
	e.server.SetMessages("c1", []json.RawMessage{
		json.RawMessage(`{"id":"7","channel_id":"c1","content":"seven","timestamp":"2026-01-01T00:07:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"6","channel_id":"c1","content":"six","timestamp":"2026-01-01T00:06:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"5","channel_id":"c1","content":"msg 5","timestamp":"2026-01-01T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"4","channel_id":"c1","content":"msg 4","timestamp":"2026-01-01T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"2","channel_id":"c1","content":"msg 2 EDITED","timestamp":"2026-01-01T00:00:00Z","edited_timestamp":"2026-01-02T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"1","channel_id":"c1","content":"msg 1","timestamp":"2026-01-01T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
	})

	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}

	if m, ok, _ := e.store.GetMessageByExternalID(e.ctx, e.channel.ID, "6"); !ok || m.Content == nil {
		t.Error("gap message 6 not filled")
	}
	if m, ok, _ := e.store.GetMessageByExternalID(e.ctx, e.channel.ID, "7"); !ok || m.Content == nil {
		t.Error("gap message 7 not filled")
	}
	if m, _, _ := e.store.GetMessageByExternalID(e.ctx, e.channel.ID, "2"); m.Content == nil || *m.Content != "msg 2 EDITED" {
		t.Errorf("edit not reconciled: %v", m.Content)
	}
	if m, _, _ := e.store.GetMessageByExternalID(e.ctx, e.channel.ID, "3"); m.DeletedAt == nil || m.Content != nil {
		t.Error("deletion not reconciled for message 3")
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.LastSyncedAt == nil || state.Status != archive.SyncStatusSynced {
		t.Errorf("state after reconcile = %+v", state)
	}
	if state.NewestExternalID == nil || *state.NewestExternalID != "7" {
		t.Errorf("newest = %v", state.NewestExternalID)
	}
	// The deletion went through the standard path, so it is ledgered.
	var ledger int
	if err := e.pool.QueryRow(e.ctx,
		"SELECT count(*) FROM deletion_ledger WHERE external_id = '3'").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != 1 {
		t.Errorf("ledger entries for reconciled deletion = %d, want 1", ledger)
	}
}

func TestReconcileSkipsIncompleteBackfill(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 5))
	// No backfill ran, so reconcile must not touch anything.
	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 0 {
		t.Errorf("reconcile imported %d messages before backfill", counts.Messages)
	}
}

func TestReconcileTransientErrorRetries(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 3))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}

	e.server.FailNextMessagesAfter(0) // the reconcile's first fetch fails
	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err == nil {
		t.Error("transient reconcile failure must surface as a job error")
	}
}

func TestReconcileForbiddenRecordsError(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 3))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}

	e.server.ForbidChannel("c1")
	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err != nil {
		t.Fatalf("403 must not surface as a retryable error, got %v", err)
	}
	state, _ := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID)
	if state.Status != archive.SyncStatusError || state.LastError == "" {
		t.Errorf("state = %+v", state)
	}
	// Nothing may be tombstoned just because the archive lost access.
	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 3 {
		t.Errorf("messages = %d, want 3 kept", counts.Messages)
	}
}

// An unparseable live message keeps the entire pass retryable. It must not be
// mistaken for a deletion, nor may other apparent gaps be committed while the
// source snapshot is incomplete.
func TestReconcileFailsSafelyWhenMessageCannotParse(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 4))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}

	// Discord still has 1, 2 and 4; message 3 is genuinely deleted. But
	// message 2 now carries a timestamp this build cannot normalize.
	e.server.SetMessages("c1", []json.RawMessage{
		json.RawMessage(`{"id":"4","channel_id":"c1","content":"msg 4","timestamp":"2026-01-01T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"2","channel_id":"c1","content":"msg 2","timestamp":"01/01/2026 00:00:00","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"1","channel_id":"c1","content":"msg 1","timestamp":"2026-01-01T00:00:00Z","type":0,"author":{"id":"u1","username":"john"}}`),
	})

	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err == nil {
		t.Fatal("unparseable message did not fail reconciliation")
	}

	m, ok, _ := e.store.GetMessageByExternalID(e.ctx, e.channel.ID, "2")
	if !ok || m.DeletedAt != nil || m.Content == nil {
		t.Errorf("unparseable but present message was tombstoned: %+v", m)
	}
	var ledger int
	if err := e.pool.QueryRow(e.ctx,
		"SELECT count(*) FROM deletion_ledger WHERE external_id = '2'").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if ledger != 0 {
		t.Errorf("ledger entries for a message Discord still returns = %d, want 0", ledger)
	}
	// The pass cannot safely conclude that any message is gone until every
	// returned payload defining the window has been understood.
	if m, _, _ := e.store.GetMessageByExternalID(e.ctx, e.channel.ID, "3"); m.DeletedAt != nil {
		t.Error("partial reconciliation tombstoned message 3")
	}
}

// With nothing parsed, the pass has no lower bound for its window and
// so must tombstone nothing at all.
func TestReconcileDetectsNoDeletionsWhenNothingParses(t *testing.T) {
	e := newEnv(t)
	e.server.SetMessages("c1", fakeMessages("c1", 4))
	if err := e.sync.HandleBackfill(e.ctx, backfillJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}

	e.server.SetMessages("c1", []json.RawMessage{
		json.RawMessage(`{"id":"4","channel_id":"c1","content":"msg 4","timestamp":"01/01/2026 00:00:00","type":0,"author":{"id":"u1","username":"john"}}`),
		json.RawMessage(`{"id":"3","channel_id":"c1","content":"msg 3","timestamp":"01/01/2026 00:00:00","type":0,"author":{"id":"u1","username":"john"}}`),
	})

	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err == nil {
		t.Fatal("entirely unparseable page did not fail reconciliation")
	}
	counts, _ := e.store.Counts(e.ctx)
	if counts.Messages != 4 {
		t.Errorf("live messages = %d, want 4 kept", counts.Messages)
	}
}

func TestReconcileContinuesBeyondFormerPageCap(t *testing.T) {
	e := newEnv(t)
	const total = 1001
	timestamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	messages := make([]json.RawMessage, 0, total)
	for i := total; i >= 1; i-- {
		messages = append(messages, json.RawMessage(fmt.Sprintf(
			`{"id":"%d","channel_id":"c1","content":"msg %d","timestamp":%q,"type":0,"author":{"id":"u1","username":"john"}}`,
			i, i, timestamp)))
	}
	e.server.SetMessages("c1", messages)
	if _, err := e.store.GetOrCreateSyncState(e.ctx, e.channel.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkBackfillComplete(e.ctx, e.channel.ID); err != nil {
		t.Fatal(err)
	}

	if err := e.sync.HandleReconcile(e.ctx, reconcileJob(t, e.channel.ID)); err != nil {
		t.Fatal(err)
	}
	counts, err := e.store.Counts(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Messages != total {
		t.Fatalf("messages = %d, want %d", counts.Messages, total)
	}
	if got := e.server.RequestCount("/channels/c1/messages"); got != 11 {
		t.Errorf("message pages = %d, want 11", got)
	}
}
