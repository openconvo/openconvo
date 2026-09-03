package backups

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/testutil"
)

type fakeDumper func(context.Context, string) error

func (f fakeDumper) Dump(ctx context.Context, destination string) error {
	return f(ctx, destination)
}

type memoryRepository struct {
	mu        sync.Mutex
	objects   map[string][]byte
	deleted   []string
	checkErr  error
	deleteErr error
	// onDelete observes the database as the object is being deleted.
	onDelete func(key string)
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{objects: make(map[string][]byte)}
}

func (r *memoryRepository) Check(context.Context) error { return r.checkErr }

func (r *memoryRepository) Put(_ context.Context, key string, body io.Reader, size int64) error {
	content, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(content)) != size {
		return errors.New("wrong size")
	}
	r.mu.Lock()
	r.objects[key] = content
	r.mu.Unlock()
	return nil
}

func (r *memoryRepository) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	r.mu.Lock()
	content, ok := r.objects[key]
	r.mu.Unlock()
	if !ok {
		return nil, 0, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(content)), int64(len(content)), nil
}

func (r *memoryRepository) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.onDelete != nil {
		r.onDelete(key)
	}
	if r.deleteErr != nil {
		return r.deleteErr
	}
	delete(r.objects, key)
	r.deleted = append(r.deleted, key)
	return nil
}

func newTestService(t *testing.T, retention int) (*Service, *memoryRepository) {
	t.Helper()
	pool := testutil.NewDB(t)
	repo := newMemoryRepository()
	queue := jobs.NewQueue(pool)
	settings := Settings{
		Enabled: true, Provider: "r2", Endpoint: "https://account.r2.cloudflarestorage.com",
		Region: "auto", Bucket: "backups", Prefix: "openconvo/db",
		IntervalHours: 24, RetentionCount: retention,
	}
	service := New(pool, queue, Options{
		Defaults: settings, Credentials: Credentials{AccessKey: "access", SecretKey: "secret"},
		DatabaseURL: "postgres://unused/unused", PGDumpPath: "pg_dump",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.repository = func(Settings) (repository, error) { return repo, nil }
	service.dumper = fakeDumper(func(_ context.Context, destination string) error {
		return os.WriteFile(destination, []byte("postgres custom dump fixture"), 0o600)
	})
	service.now = func() time.Time { return time.Date(2026, 8, 20, 3, 4, 5, 0, time.UTC) }
	return service, repo
}

func TestServiceBackupLifecycleAndRetention(t *testing.T) {
	service, repo := newTestService(t, 1)
	ctx := context.Background()

	view, err := service.SaveSettings(ctx, service.defaults)
	if err != nil {
		t.Fatal(err)
	}
	if view.Source != "dashboard" || !view.CredentialsConfigured {
		t.Errorf("settings view = %+v", view)
	}
	var stored string
	if err := service.pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key = $1`, settingsKey).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(stored), []byte("secret")) || bytes.Contains([]byte(stored), []byte("access")) {
		t.Fatalf("credentials persisted in settings: %s", stored)
	}

	first, created, err := service.RequestBackup(ctx, "manual")
	if err != nil || !created {
		t.Fatalf("RequestBackup = %+v, %v, %v", first, created, err)
	}
	active, created, err := service.RequestBackup(ctx, "manual")
	if err != nil || created || active.ID != first.ID {
		t.Fatalf("duplicate RequestBackup = %+v, %v, %v", active, created, err)
	}
	payload, _ := json.Marshal(backupPayload{BackupID: first.ID})
	if err := service.HandleDatabaseBackup(ctx, &jobs.Job{Payload: payload, Attempts: 1, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	_, _ = service.pool.Exec(ctx, `UPDATE jobs SET status = 'succeeded' WHERE kind = $1`, JobDatabaseBackup)

	items, err := service.ListBackups(ctx, 50)
	if err != nil || len(items) != 1 || items[0].Status != "succeeded" || items[0].Size == 0 || len(items[0].SHA256) != 64 {
		t.Fatalf("ListBackups = %+v, %v", items, err)
	}
	backup, body, err := service.OpenBackup(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(body)
	body.Close()
	if backup.ID != first.ID || string(content) != "postgres custom dump fixture" {
		t.Errorf("download = %+v %q", backup, content)
	}

	service.now = func() time.Time { return time.Date(2026, 8, 21, 3, 4, 5, 0, time.UTC) }
	second, created, err := service.RequestBackup(ctx, "scheduled")
	if err != nil || !created {
		t.Fatalf("second RequestBackup = %+v, %v, %v", second, created, err)
	}
	var statusWhenDeleted string
	repo.onDelete = func(string) {
		if err := service.pool.QueryRow(ctx, `SELECT status FROM database_backups WHERE id = $1::uuid`, first.ID).Scan(&statusWhenDeleted); err != nil {
			t.Error(err)
		}
	}
	payload, _ = json.Marshal(backupPayload{BackupID: second.ID})
	if err := service.HandleDatabaseBackup(ctx, &jobs.Job{Payload: payload, Attempts: 1, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != first.ObjectKey {
		t.Errorf("expired objects = %v, want %s", repo.deleted, first.ObjectKey)
	}
	if statusWhenDeleted != "expired" {
		t.Errorf("run was %q while its object was being deleted; a row must never outlive its object", statusWhenDeleted)
	}
	if _, _, err := service.OpenBackup(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenBackup expired = %v, want ErrNotFound", err)
	}
}

// A retention delete that fails must leave the run downloadable: the database
// may never describe an object that is gone, and the object is still there.
func TestRetentionKeepsRunUsableWhenObjectDeleteFails(t *testing.T) {
	service, repo := newTestService(t, 1)
	ctx := context.Background()

	first, _, err := service.RequestBackup(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	runBackup(t, service, first.ID)

	repo.deleteErr = errors.New("bucket unreachable")
	service.now = func() time.Time { return time.Date(2026, 8, 21, 3, 4, 5, 0, time.UTC) }
	second, _, err := service.RequestBackup(ctx, "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	runBackup(t, service, second.ID)

	items, err := service.ListBackups(ctx, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("ListBackups = %+v, %v", items, err)
	}
	_, body, err := service.OpenBackup(ctx, first.ID)
	if err != nil {
		t.Fatalf("run whose object survived retention is not downloadable: %v", err)
	}
	body.Close()

	// The next pass retries the delete now that the destination answers.
	repo.deleteErr = nil
	service.now = func() time.Time { return time.Date(2026, 8, 22, 3, 4, 5, 0, time.UTC) }
	third, _, err := service.RequestBackup(ctx, "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	runBackup(t, service, third.ID)
	if _, _, err := service.OpenBackup(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenBackup expired = %v, want ErrNotFound", err)
	}
}

// A duplicate suppressed by the job queue is not a failed backup.
func TestSuppressedEnqueueDoesNotRecordFailedRun(t *testing.T) {
	service, _ := newTestService(t, 3)
	ctx := context.Background()

	first, _, err := service.RequestBackup(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(backupPayload{BackupID: first.ID})
	if err := service.HandleDatabaseBackup(ctx, &jobs.Job{Payload: payload, Attempts: 1, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}

	// The run is finished while its job row is still pending, which is the
	// window in which a second request has its enqueue deduplicated away.
	backup, created, err := service.RequestBackup(ctx, "manual")
	if created || err == nil {
		t.Fatalf("suppressed enqueue = %+v, %v, %v", backup, created, err)
	}
	items, err := service.ListBackups(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != first.ID || items[0].Status != "succeeded" {
		t.Fatalf("suppressed duplicate left a run behind: %+v", items)
	}
}

func TestFailedAttemptStaysActiveUntilFinalRetry(t *testing.T) {
	service, _ := newTestService(t, 3)
	service.dumper = fakeDumper(func(context.Context, string) error { return errors.New("disk full") })
	ctx := context.Background()
	backup, _, err := service.RequestBackup(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(backupPayload{BackupID: backup.ID})
	if err := service.HandleDatabaseBackup(ctx, &jobs.Job{Payload: payload, Attempts: 1, MaxAttempts: 3}); err == nil {
		t.Fatal("failed dump returned nil")
	}
	items, err := service.ListBackups(ctx, 10)
	if err != nil || len(items) != 1 || items[0].Status != "pending" || items[0].Error != "disk full" {
		t.Fatalf("after retryable error: %+v, %v", items, err)
	}
	if err := service.HandleDatabaseBackup(ctx, &jobs.Job{Payload: payload, Attempts: 3, MaxAttempts: 3}); err == nil {
		t.Fatal("final failed dump returned nil")
	}
	items, _ = service.ListBackups(ctx, 10)
	if items[0].Status != "failed" || items[0].CompletedAt == nil {
		t.Fatalf("after final error: %+v", items[0])
	}
}

func TestPanickingAttemptDoesNotLeaveRunStuckRunning(t *testing.T) {
	service, _ := newTestService(t, 3)
	service.dumper = fakeDumper(func(context.Context, string) error { panic("broken dumper") })
	ctx := context.Background()
	backup, _, err := service.RequestBackup(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(backupPayload{BackupID: backup.ID})
	func() {
		defer func() { _ = recover() }()
		_ = service.HandleDatabaseBackup(ctx, &jobs.Job{Payload: payload, Attempts: 1, MaxAttempts: 3})
	}()
	items, err := service.ListBackups(ctx, 10)
	if err != nil || len(items) != 1 || items[0].Status != "pending" || items[0].Error != "panic: broken dumper" {
		t.Fatalf("after panic: %+v, %v", items, err)
	}
}

// runBackup executes one queued run and finalises its job, so the next request
// is not deduplicated away by a job row this test never claimed.
func runBackup(t *testing.T, service *Service, id string) {
	t.Helper()
	payload, _ := json.Marshal(backupPayload{BackupID: id})
	if err := service.HandleDatabaseBackup(context.Background(), &jobs.Job{Payload: payload, Attempts: 1, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.pool.Exec(context.Background(),
		`UPDATE jobs SET status = 'succeeded' WHERE kind = $1`, JobDatabaseBackup); err != nil {
		t.Fatal(err)
	}
}
