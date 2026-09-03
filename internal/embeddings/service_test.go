package embeddings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/testutil"
)

type fakeEmbedder struct {
	calls  [][]string
	err    error
	reject map[string]bool
	before func()
	byText map[string][]float32
}

func (f *fakeEmbedder) Embed(_ context.Context, input []string) ([][]float32, error) {
	f.calls = append(f.calls, append([]string(nil), input...))
	if f.before != nil {
		f.before()
	}
	if f.err != nil {
		return nil, f.err
	}
	// Mirror the real client: a request is all-or-nothing, and one input it
	// will not accept — blank once trimmed, or refused on its own merits —
	// rejects the whole batch, every time it is sent.
	for _, value := range input {
		if strings.TrimSpace(value) == "" || f.reject[value] {
			return nil, fmt.Errorf("%w: fake rejection", ErrRejectedInput)
		}
	}
	out := make([][]float32, len(input))
	for i := range out {
		if vector, ok := f.byText[input[i]]; ok {
			out[i] = append([]float32(nil), vector...)
		} else {
			out[i] = testVector(float32(i + 1))
		}
	}
	return out, nil
}

func TestSemanticSearchRanksAndFiltersDerivedMessages(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	provider := &fakeEmbedder{byText: map[string][]float32{
		"Use hide glue for the maple veneer.": vectorAxes(1, 0),
		"Acrylic paint dries quickly.":        vectorAxes(0, 1),
		"advice for bonding wood":             vectorAxes(1, 0.05),
	}}
	service := New(pool, jobs.NewQueue(pool), Options{Defaults: Preset(true), APIKey: "configured"}, nil)
	service.embedder = provider
	if err := service.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	store := archive.New(pool)
	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "semantic-guild", Name: "Woodworkers",
	})
	if err != nil {
		t.Fatal(err)
	}
	wood, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID, ExternalID: "semantic-wood", Kind: "text", Name: "wood",
	})
	if err != nil {
		t.Fatal(err)
	}
	paint, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID, ExternalID: "semantic-paint", Kind: "text", Name: "paint",
	})
	if err != nil {
		t.Fatal(err)
	}
	john, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source: archive.SourceDiscord, ExternalID: "semantic-john", Username: "john", DisplayName: "John",
	})
	if err != nil {
		t.Fatal(err)
	}
	alex, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source: archive.SourceDiscord, ExternalID: "semantic-alex", Username: "alex", DisplayName: "Alex",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	glueText := "Use hide glue for the maple veneer."
	glue, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: wood.ID, ActorID: &john.ID, ExternalID: "semantic-glue",
		Content: &glueText, SourceCreatedAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAttachment(ctx, archive.AttachmentUpsert{
		MessageID: glue.ID, ExternalID: "semantic-attachment", Filename: "veneer.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	paintText := "Acrylic paint dries quickly."
	paintMessage, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: paint.ID, ActorID: &alex.ID, ExternalID: "semantic-paint-message",
		Content: &paintText, SourceCreatedAt: base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	page, err := service.SearchMessages(ctx, archive.SearchParams{
		Query: "advice for bonding wood", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Results) != 1 || !page.HasMore || page.Results[0].MessageID != glue.ID {
		t.Fatalf("semantic page = %+v", page)
	}
	if page.Results[0].Actor == nil || page.Results[0].Actor.DisplayName != "John" ||
		page.Results[0].ChannelName != "wood" || !page.Results[0].HasAttachment ||
		strings.Contains(page.Results[0].Excerpt, "<mark>") {
		t.Fatalf("semantic result = %+v", page.Results[0])
	}

	attachmentOnly := true
	before := base.Add(12 * time.Hour)
	filtered, err := service.SearchMessages(ctx, archive.SearchParams{
		Query: "advice for bonding wood", ChannelID: wood.ID, Author: "JOHN",
		Before: &before, HasAttachment: &attachmentOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Results) != 1 || filtered.Results[0].MessageID != glue.ID {
		t.Fatalf("filtered semantic results = %+v", filtered.Results)
	}

	withoutAttachment := false
	after := base.Add(12 * time.Hour)
	filtered, err = service.SearchMessages(ctx, archive.SearchParams{
		Query: "advice for bonding wood", After: &after, HasAttachment: &withoutAttachment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Results) != 1 || filtered.Results[0].MessageID != paintMessage.ID {
		t.Fatalf("negative attachment semantic results = %+v", filtered.Results)
	}
	if got := provider.calls[len(provider.calls)-1]; len(got) != 1 || got[0] != "advice for bonding wood" {
		t.Fatalf("query embedding input = %#v", got)
	}
}

func TestSemanticSearchRequiresEnabledReadyProvider(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	disabled := New(pool, queue, Options{Defaults: Preset(false), APIKey: "configured"}, nil)
	disabled.embedder = &fakeEmbedder{}
	if _, err := disabled.SearchMessages(ctx, archive.SearchParams{Query: "query"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled search error = %v", err)
	}

	notConfigured := New(pool, queue, Options{Defaults: Preset(true)}, nil)
	if _, err := notConfigured.SearchMessages(ctx, archive.SearchParams{Query: "query"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured search error = %v", err)
	}

	ready := New(pool, queue, Options{Defaults: Preset(true), APIKey: "configured"}, nil)
	ready.embedder = &fakeEmbedder{}
	if _, err := ready.ensureGeneration(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ready.SearchMessages(ctx, archive.SearchParams{Query: "query"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("building search error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE derived.embedding_generations SET status='active'`); err != nil {
		t.Fatal(err)
	}
	ready.embedder = &fakeEmbedder{err: errors.New("provider unavailable")}
	if _, err := ready.SearchMessages(ctx, archive.SearchParams{Query: "query"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("provider search error = %v", err)
	}
}

func TestMessageEmbeddingLifecycle(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)
	provider := &fakeEmbedder{}
	service := New(pool, queue, Options{Defaults: Preset(true), APIKey: "configured"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.embedder = provider
	if err := service.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	store := archive.New(pool)
	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{Source: archive.SourceDiscord, ExternalID: "guild"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{CommunityID: community.ID, ExternalID: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	content := "original message"
	message, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ExternalID: "message", Content: &content, SourceCreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	assertEmbeddingCount(t, pool, message.ID, 1)
	if len(provider.calls) != 1 || provider.calls[0][0] != content {
		t.Fatalf("provider calls = %#v", provider.calls)
	}

	// A duplicate canonical event with identical content does not invalidate or
	// resend the vector.
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ExternalID: "message", Content: &content, SourceCreatedAt: createdAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("duplicate event caused provider call: %d", len(provider.calls))
	}

	edited := "edited message"
	if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ExternalID: "message", Content: &edited, SourceCreatedAt: createdAt,
	}); err != nil {
		t.Fatal(err)
	}
	assertEmbeddingCount(t, pool, message.ID, 0)
	payload, _ := json.Marshal(messagePayload{MessageID: message.ID})
	if err := service.handleMessage(ctx, &jobs.Job{Payload: payload}); err != nil {
		t.Fatal(err)
	}
	assertEmbeddingCount(t, pool, message.ID, 1)
	if provider.calls[len(provider.calls)-1][0] != edited {
		t.Fatalf("edited input = %#v", provider.calls[len(provider.calls)-1])
	}

	if _, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, message.ExternalID, message.SourceCreatedAt); err != nil {
		t.Fatal(err)
	}
	assertEmbeddingCount(t, pool, message.ID, 0)
}

func TestChangedMessageCannotCommitStaleEmbedding(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	store := archive.New(pool)
	community, _ := store.UpsertCommunity(ctx, archive.CommunityUpsert{Source: archive.SourceDiscord, ExternalID: "guild"})
	channel, _ := store.UpsertChannel(ctx, archive.ChannelUpsert{CommunityID: community.ID, ExternalID: "channel"})
	createdAt := time.Now().UTC()
	original := "old private value"
	message, err := store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID: channel.ID, ExternalID: "message", Content: &original, SourceCreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	provider := &fakeEmbedder{}
	service := New(pool, jobs.NewQueue(pool), Options{Defaults: Preset(true), APIKey: "configured"}, nil)
	service.embedder = provider
	if err := service.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	provider.before = func() {
		provider.before = nil
		updated := "new value"
		if _, err := store.UpsertMessage(ctx, archive.MessageUpsert{
			ChannelID: channel.ID, ExternalID: "message", Content: &updated, SourceCreatedAt: createdAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(messagePayload{MessageID: message.ID})
	err = service.handleMessage(ctx, &jobs.Job{Payload: payload})
	if err == nil {
		t.Fatal("content race succeeded instead of retrying")
	}
	assertEmbeddingCount(t, pool, message.ID, 0)
}

func TestNormalizeSettingsAllowsOnlyPreset(t *testing.T) {
	if _, err := normalizeSettings(Preset(true)); err != nil {
		t.Fatal(err)
	}
	wrong := Preset(true)
	wrong.Dimensions = 1536
	if _, err := normalizeSettings(wrong); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("wrong dimensions error = %v", err)
	}
}

func assertEmbeddingCount(t *testing.T, pool *pgxpool.Pool, messageID string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM derived.message_embeddings WHERE message_id=$1::uuid`, messageID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("embedding count = %d, want %d", got, want)
	}
}

func vectorAxes(first, second float32) []float32 {
	vector := make([]float32, Dimensions)
	vector[0] = first
	vector[1] = second
	return vector
}

type sweepFixture struct {
	service  *Service
	store    *archive.Store
	pool     *pgxpool.Pool
	channel  string
	provider *fakeEmbedder
}

func newSweepFixture(t *testing.T, provider *fakeEmbedder) sweepFixture {
	t.Helper()
	pool := testutil.NewDB(t)
	ctx := context.Background()
	service := New(pool, jobs.NewQueue(pool), Options{Defaults: Preset(true), APIKey: "configured"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.embedder = provider
	if err := service.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	store := archive.New(pool)
	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "guild",
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID, ExternalID: "channel",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sweepFixture{service: service, store: store, pool: pool, channel: channel.ID, provider: provider}
}

func (f sweepFixture) message(t *testing.T, externalID, content string) archive.Message {
	t.Helper()
	message, err := f.store.UpsertMessage(context.Background(), archive.MessageUpsert{
		ChannelID: f.channel, ExternalID: externalID, Content: &content, SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func (f sweepFixture) sentInputs() []string {
	var sent []string
	for _, call := range f.provider.calls {
		sent = append(sent, call...)
	}
	return sent
}

// A message that is visually blank on Discord trims to empty for the provider
// but not for PostgreSQL's single-argument btrim. Selecting it as a candidate
// used to fail its whole batch, and because candidates are read in id order,
// the same batch came back on every retry and every later sweep: nothing in
// the archive was ever embedded again.
func TestBlankMessageDoesNotStallSweep(t *testing.T) {
	fixture := newSweepFixture(t, &fakeEmbedder{})
	ctx := context.Background()
	// Newline, tab and a non-breaking space.
	blank := fixture.message(t, "blank", string([]rune{0x0A, 0x09, 0x00A0}))
	indexed := fixture.message(t, "real", "a message worth embedding")

	if err := fixture.service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatalf("sweep failed on a blank message: %v", err)
	}
	assertEmbeddingCount(t, fixture.pool, indexed.ID, 1)
	assertEmbeddingCount(t, fixture.pool, blank.ID, 0)
	for _, input := range fixture.sentInputs() {
		if strings.TrimSpace(input) == "" {
			t.Fatalf("blank content was sent to the provider: %q", input)
		}
	}

	// The index still becomes usable, and the dashboard must not report a
	// message that can never be embedded as outstanding work.
	view, err := fixture.service.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.GenerationStatus != "active" {
		t.Fatalf("generation status = %q", view.GenerationStatus)
	}
	if view.EligibleMessages != 1 || view.EmbeddedMessages != 1 {
		t.Fatalf("embedded %d of %d eligible", view.EmbeddedMessages, view.EligibleMessages)
	}

	// A later sweep must still converge rather than retry the blank message
	// forever.
	before := len(fixture.provider.calls)
	if err := fixture.service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.provider.calls) != before {
		t.Fatalf("second sweep made %d provider calls", len(fixture.provider.calls)-before)
	}
}

// Any input the provider refuses for good — oversized, malformed, refused on
// policy — fails the whole all-or-nothing request. It must not take the rest
// of the archive down with it.
func TestRejectedMessageDoesNotBlockLaterMessages(t *testing.T) {
	poison := "the provider will never accept this"
	fixture := newSweepFixture(t, &fakeEmbedder{reject: map[string]bool{poison: true}})
	ctx := context.Background()
	first := fixture.message(t, "first", "a message before the poison row")
	rejected := fixture.message(t, "poison", poison)
	last := fixture.message(t, "last", "a message after the poison row")

	if err := fixture.service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatalf("one rejected message failed the sweep: %v", err)
	}
	assertEmbeddingCount(t, fixture.pool, first.ID, 1)
	assertEmbeddingCount(t, fixture.pool, last.ID, 1)
	assertEmbeddingCount(t, fixture.pool, rejected.ID, 0)

	generation, found, err := fixture.service.generation(ctx)
	if err != nil || !found {
		t.Fatalf("generation lookup: %v, found %v", err, found)
	}
	if generation.Status != "active" {
		t.Fatalf("generation status = %q", generation.Status)
	}

	// The next sweep offers the rejected message once more — it may have been
	// edited since — and still finishes.
	if err := fixture.service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatal(err)
	}
	assertEmbeddingCount(t, fixture.pool, rejected.ID, 0)

	// A single-message job must not burn its retries on it either.
	payload, _ := json.Marshal(messagePayload{MessageID: rejected.ID})
	if err := fixture.service.handleMessage(ctx, &jobs.Job{Payload: payload}); err != nil {
		t.Fatalf("rejected message failed its job: %v", err)
	}
}

// Turning embeddings off must stop content leaving the machine, even while a
// first full-archive backfill is midway through its unbounded loop.
func TestDisablingEmbeddingsStopsSweepAtTheNextBatch(t *testing.T) {
	fixture := newSweepFixture(t, &fakeEmbedder{})
	ctx := context.Background()
	for i := 0; i <= sweepBatchSize; i++ {
		fixture.message(t, fmt.Sprintf("bulk-%03d", i), fmt.Sprintf("archived message %d", i))
	}
	fixture.provider.before = func() {
		fixture.provider.before = nil
		if _, err := fixture.service.SaveSettings(ctx, Preset(false)); err != nil {
			t.Errorf("disable embeddings: %v", err)
		}
	}

	if err := fixture.service.handleSweep(ctx, &jobs.Job{}); err != nil {
		t.Fatalf("disabled sweep returned an error: %v", err)
	}
	if len(fixture.provider.calls) != 1 {
		t.Fatalf("provider calls after disabling = %d, want 1", len(fixture.provider.calls))
	}
	if got := len(fixture.sentInputs()); got != sweepBatchSize {
		t.Fatalf("messages sent = %d, want the one batch already in flight (%d)", got, sweepBatchSize)
	}
	generation, found, err := fixture.service.generation(ctx)
	if err != nil || !found {
		t.Fatalf("generation lookup: %v, found %v", err, found)
	}
	if generation.Status == "active" {
		t.Fatal("interrupted sweep activated an incomplete generation")
	}
}

// Deletion is honoured at send time, not only at store time: a tombstone that
// lands after the candidate rows were read must stop the content of that
// message from reaching the provider at all.
func TestDeletedMessageContentIsNotSentToProvider(t *testing.T) {
	fixture := newSweepFixture(t, &fakeEmbedder{})
	ctx := context.Background()
	secret := "a message the community deleted"
	deleted := fixture.message(t, "deleted", secret)
	live := fixture.message(t, "live", "a message that is still archived")
	generation, err := fixture.service.ensureGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.MarkMessageDeleted(ctx, archive.SourceDiscord, fixture.channel, "deleted", deleted.SourceCreatedAt); err != nil {
		t.Fatal(err)
	}

	// The rows the sweep is holding still carry the pre-deletion content.
	result, err := fixture.service.embedRows(ctx, generation.ID, []messageRow{
		{ID: deleted.ID, Content: secret},
		{ID: live.ID, Content: "a message that is still archived"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.stored != 1 {
		t.Fatalf("stored %d vectors, want 1", result.stored)
	}
	for _, input := range fixture.sentInputs() {
		if input == secret {
			t.Fatal("content of a deleted message was sent to the provider")
		}
	}
	assertEmbeddingCount(t, fixture.pool, deleted.ID, 0)
	assertEmbeddingCount(t, fixture.pool, live.ID, 1)
}
