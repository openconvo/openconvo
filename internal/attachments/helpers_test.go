package attachments_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/attachments"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/storage"
	"github.com/openconvo/openconvo/internal/testutil"
)

func strPtr(s string) *string { return &s }

// fakeRefresher stands in for Discord. It records what it was asked for
// so tests can assert that refreshing happened only when it should.
type fakeRefresher struct {
	replacement string
	calls       int
	err         error
	// keyAs replaces the key the reply is filed under, standing in for a
	// Discord that echoes the original URL in a normalized form rather
	// than byte for byte.
	keyAs string
	// empty answers a refresh with no entries at all.
	empty bool
}

func (f *fakeRefresher) RefreshAttachmentURLs(_ context.Context, urls []string) (map[string]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	if f.empty {
		return out, nil
	}
	for _, u := range urls {
		key := u
		if f.keyAs != "" {
			key = f.keyAs
		}
		out[key] = f.replacement
	}
	return out, nil
}

// syncBuffer collects log output for tests that assert on what the
// pipeline told the operator. A mutex because worker goroutines log
// concurrently with the test reading.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fixture struct {
	ctx       context.Context
	db        *pgxpool.Pool
	store     *archive.Store
	blobs     storage.Store
	queue     *jobs.Queue
	refresher *fakeRefresher
	channel   archive.Channel
	actor     archive.Actor
	logs      *syncBuffer
}

// pool exposes the database for assertions that are clearer as SQL than
// as store calls.
func (f *fixture) pool() *pgxpool.Pool { return f.db }

// attachmentStatus reads an attachment's download_status directly, for
// assertions that care about state the store's own read methods don't
// expose as a single field.
func (f *fixture) attachmentStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := f.pool().QueryRow(f.ctx,
		`SELECT download_status FROM attachments WHERE id = $1::uuid`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// attachmentError reads an attachment's download_error directly.
func (f *fixture) attachmentError(t *testing.T, id string) string {
	t.Helper()
	var reason *string
	if err := f.pool().QueryRow(f.ctx,
		`SELECT download_error FROM attachments WHERE id = $1::uuid`, id).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

// countBlobs counts the rows in the blobs table, so tests can assert
// that a rejected file never reached the blob store.
func countBlobs(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.pool().QueryRow(f.ctx, `SELECT count(*) FROM blobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ageBlobs backdates every blob past the reclamation grace period.
func (f *fixture) ageBlobs(t *testing.T) {
	t.Helper()
	if _, err := f.pool().Exec(f.ctx,
		`UPDATE blobs SET created_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
}

// failingBlobStore is a storage.Store whose writes always fail, standing
// in for a full disk.
type failingBlobStore struct{ err error }

func (s failingBlobStore) Put(context.Context, io.Reader) (storage.PutResult, error) {
	return storage.PutResult{}, s.err
}
func (s failingBlobStore) Open(context.Context, string) (io.ReadCloser, error) { return nil, s.err }
func (s failingBlobStore) Exists(context.Context, string) (bool, error)        { return false, nil }
func (s failingBlobStore) Delete(context.Context, string) error                { return s.err }

// newFixture builds a fixture backed by a real filesystem blob store.
func newFixture(t *testing.T, opts attachments.Options) (*fixture, *attachments.Downloader) {
	t.Helper()
	blobs, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWithBlobs(t, opts, blobs)
}

// newFixtureWithBlobs lets a test substitute the blob store, which is
// how storage failures are exercised.
func newFixtureWithBlobs(t *testing.T, opts attachments.Options, blobs storage.Store) (*fixture, *attachments.Downloader) {
	t.Helper()
	pool := testutil.NewDB(t)
	ctx := context.Background()

	store := archive.New(pool)
	refresher := &fakeRefresher{replacement: "unset"}
	queue := jobs.NewQueue(pool)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

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
	// The sweep only offers files from channels the operator enabled, and
	// a channel holding archived messages is an enabled one.
	if err := store.SetChannelArchiveEnabled(ctx, channel.ID, true); err != nil {
		t.Fatal(err)
	}
	actor, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source: archive.SourceDiscord, ExternalID: "u1", Username: "someone",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{ctx: ctx, db: pool, store: store, blobs: blobs, queue: queue,
		refresher: refresher, channel: channel, actor: actor, logs: logs}
	return f, attachments.New(store, blobs, queue, refresher, opts, logger)
}

// addAttachment creates a message with one pending attachment and
// returns the attachment ID.
func (f *fixture) addAttachment(t *testing.T, externalID, url string, size int64) string {
	t.Helper()
	msg, err := f.store.UpsertMessage(f.ctx, archive.MessageUpsert{
		ChannelID: f.channel.ID, ActorID: strPtr(f.actor.ID), ExternalID: externalID,
		Content: strPtr("here"), SourceCreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.store.UpsertAttachment(f.ctx, archive.AttachmentUpsert{
		MessageID: msg.ID, ExternalID: externalID + "-a", Filename: "file.bin",
		ContentType: "application/octet-stream", Size: size, SourceURL: url,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// logLinesContaining counts the log lines mentioning substr, for tests
// that care how often the pipeline said something as much as whether it
// said it at all.
func (f *fixture) logLinesContaining(substr string) int {
	n := 0
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// countJobs counts queued jobs of one kind, for assertions that a sweep
// enqueued exactly as many jobs as expected — no more, no fewer.
func (f *fixture) countJobs(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.pool().QueryRow(f.ctx,
		`SELECT count(*) FROM jobs WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// downloadJob builds a download job as the queue would deliver it.
func downloadJob(attachmentID string, attempt, maxAttempts int) *jobs.Job {
	return &jobs.Job{
		Kind:        attachments.JobDownload,
		Payload:     []byte(`{"attachment_id":"` + attachmentID + `"}`),
		Attempts:    attempt,
		MaxAttempts: maxAttempts,
	}
}
