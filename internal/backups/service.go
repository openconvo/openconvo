package backups

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/jobs"
)

const (
	backupMaxAttempts = 3
	schedulerTick     = time.Minute
)

// Options wires the backup service without adding secrets to persisted
// settings. Defaults are used until an administrator saves settings in the app.
type Options struct {
	Defaults    Settings
	Credentials Credentials
	DatabaseURL string
	PGDumpPath  string
}

// Service owns database backup configuration, scheduling, and execution.
type Service struct {
	pool        *pgxpool.Pool
	queue       *jobs.Queue
	defaults    Settings
	credentials Credentials
	dumper      dumper
	repository  repositoryFactory
	logger      *slog.Logger
	now         func() time.Time
}

// New creates the production backup service.
func New(pool *pgxpool.Pool, queue *jobs.Queue, opts Options, logger *slog.Logger) *Service {
	credentialsValue := opts.Credentials
	return &Service{
		pool:        pool,
		queue:       queue,
		defaults:    opts.Defaults,
		credentials: credentialsValue,
		dumper:      pgDumper{binary: opts.PGDumpPath, databaseURL: opts.DatabaseURL},
		repository: func(settings Settings) (repository, error) {
			return newS3Repository(settings, credentialsValue)
		},
		logger: logger.With("component", "backups"),
		now:    time.Now,
	}
}

// RegisterHandlers gives the database backup job to its dedicated one-slot
// worker. No other worker may claim this kind.
func (s *Service) RegisterHandlers(worker *jobs.Worker) {
	worker.Register(JobDatabaseBackup, s.HandleDatabaseBackup)
}

// GetSettings returns administrator-saved settings when present, otherwise environment
// defaults. Credentials are reported only as a boolean.
func (s *Service) GetSettings(ctx context.Context) (SettingsView, error) {
	settings, source, err := s.effectiveSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	return SettingsView{
		Settings:              settings,
		CredentialsConfigured: s.credentials.Configured(),
		Source:                source,
	}, nil
}

// SaveSettings validates and persists non-secret destination and schedule
// settings. Enabling performs a read-only bucket check before committing.
func (s *Service) SaveSettings(ctx context.Context, input Settings) (SettingsView, error) {
	settings, err := normalizeSettings(input, input.Enabled)
	if err != nil {
		return SettingsView{}, err
	}
	if settings.Enabled {
		if !s.credentials.Configured() {
			return SettingsView{}, fmt.Errorf("%w: set BACKUP_S3_ACCESS_KEY and BACKUP_S3_SECRET_KEY", ErrNotConfigured)
		}
		repo, err := s.repository(settings)
		if err != nil {
			return SettingsView{}, err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = repo.Check(checkCtx)
		cancel()
		if err != nil {
			s.logger.Warn("backup destination check failed", "provider", settings.Provider, "bucket", settings.Bucket, "error", err)
			return SettingsView{}, fmt.Errorf("%w: check endpoint, bucket, region, and credentials", ErrDestination)
		}
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return SettingsView{}, fmt.Errorf("encode backup settings: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, settingsKey, body)
	if err != nil {
		return SettingsView{}, fmt.Errorf("save backup settings: %w", err)
	}
	return SettingsView{Settings: settings, CredentialsConfigured: s.credentials.Configured(), Source: "dashboard"}, nil
}

func (s *Service) effectiveSettings(ctx context.Context) (Settings, string, error) {
	var body []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, settingsKey).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		settings, normalizeErr := normalizeSettings(s.defaults, s.defaults.Enabled)
		if normalizeErr != nil {
			return Settings{}, "", fmt.Errorf("environment backup settings: %w", normalizeErr)
		}
		return settings, "environment", nil
	}
	if err != nil {
		return Settings{}, "", fmt.Errorf("load backup settings: %w", err)
	}
	var settings Settings
	if err := json.Unmarshal(body, &settings); err != nil {
		return Settings{}, "", fmt.Errorf("decode backup settings: %w", err)
	}
	settings, err = normalizeSettings(settings, settings.Enabled)
	if err != nil {
		return Settings{}, "", fmt.Errorf("stored backup settings: %w", err)
	}
	return settings, "dashboard", nil
}

// RequestBackup creates a run and enqueues it. If another run is already
// active, that run is returned and created is false; if the previous run's job
// has not been finalised yet, no run is created and the error says so.
func (s *Service) RequestBackup(ctx context.Context, trigger string) (backup Backup, created bool, err error) {
	if trigger != "manual" && trigger != "scheduled" {
		return Backup{}, false, fmt.Errorf("invalid backup trigger %q", trigger)
	}
	settings, _, err := s.effectiveSettings(ctx)
	if err != nil {
		return Backup{}, false, err
	}
	settings, err = normalizeSettings(settings, true)
	if err != nil || !s.credentials.Configured() {
		return Backup{}, false, ErrNotConfigured
	}

	createdAt := s.now().UTC()
	suffix, err := randomSuffix()
	if err != nil {
		return Backup{}, false, err
	}
	key := objectKey(settings.Prefix, createdAt, suffix)
	row := s.pool.QueryRow(ctx, `
		INSERT INTO database_backups
			(trigger, status, provider, endpoint, region, bucket, prefix, force_path_style, retention_count, object_key, created_at, updated_at)
		VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		RETURNING `+backupColumns,
		trigger, settings.Provider, settings.Endpoint, settings.Region, settings.Bucket,
		settings.Prefix, settings.ForcePathStyle, settings.RetentionCount, key, createdAt)
	backup, err = scanBackup(row, s.credentials.Configured())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			active, activeErr := s.activeBackup(ctx)
			return active, false, activeErr
		}
		return Backup{}, false, fmt.Errorf("create backup run: %w", err)
	}

	jobID, err := s.queue.Enqueue(ctx, JobDatabaseBackup, backupPayload{BackupID: backup.ID},
		jobs.WithDedupeKey(JobDatabaseBackup), jobs.WithMaxAttempts(backupMaxAttempts))
	if err != nil {
		_ = s.markFailed(context.WithoutCancel(ctx), backup.ID, err.Error())
		return Backup{}, false, err
	}
	if jobID == "" {
		// The dedupe key suppressed the enqueue: the previous run's job has
		// not been finalised yet, so this run would never be claimed. Nothing
		// has failed, so the run is discarded rather than recorded as a
		// failure the operator has to interpret. Leaving it pending would
		// block every later request through the one-active index, so a run
		// that cannot be deleted is marked failed after all.
		discardCtx := context.WithoutCancel(ctx)
		if _, deleteErr := s.pool.Exec(discardCtx,
			`DELETE FROM database_backups WHERE id = $1::uuid AND status = 'pending'`, backup.ID); deleteErr != nil {
			s.logger.Warn("discard superseded backup run", "backup_id", backup.ID, "error", deleteErr)
			_ = s.markFailed(discardCtx, backup.ID, "a database backup job was already queued")
		}
		return Backup{}, false, errors.New("a database backup job is already queued")
	}
	return backup, true, nil
}

type backupPayload struct {
	BackupID string `json:"backup_id"`
}

// HandleDatabaseBackup creates one custom-format dump, hashes it, uploads it,
// and only then publishes the successful run state.
func (s *Service) HandleDatabaseBackup(ctx context.Context, job *jobs.Job) (runErr error) {
	var payload backupPayload
	if err := job.UnmarshalPayload(&payload); err != nil {
		return fmt.Errorf("decode database backup job: %w", err)
	}
	if payload.BackupID == "" {
		return fmt.Errorf("decode database backup job: backup_id is required")
	}
	backup, err := s.getBackup(ctx, payload.BackupID, true)
	if err != nil {
		return err
	}
	if backup.Status == "succeeded" || backup.Status == "expired" {
		return nil
	}
	if err := s.markRunning(ctx, backup.ID); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = s.markAttemptFailed(context.WithoutCancel(ctx), backup.ID,
				fmt.Sprintf("panic: %v", recovered), job.Attempts >= job.MaxAttempts)
			panic(recovered)
		}
		if runErr != nil {
			_ = s.markAttemptFailed(context.WithoutCancel(ctx), backup.ID, runErr.Error(), job.Attempts >= job.MaxAttempts)
		}
	}()

	tmp, err := os.CreateTemp("", "openconvo-db-backup-*.dump")
	if err != nil {
		return fmt.Errorf("create backup staging file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close backup staging file: %w", err)
	}
	defer os.Remove(tmpPath)

	if err := s.dumper.Dump(ctx, tmpPath); err != nil {
		return err
	}
	file, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open database backup: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect database backup: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("pg_dump produced an empty backup")
	}
	digest, err := hashFile(file)
	if err != nil {
		return err
	}

	settings := settingsFromBackup(backup)
	repo, err := s.repository(settings)
	if err != nil {
		return err
	}
	if err := repo.Put(ctx, backup.ObjectKey, file, info.Size()); err != nil {
		return err
	}
	if err := s.markSucceeded(ctx, backup.ID, info.Size(), digest); err != nil {
		return err
	}
	s.expireOldBackups(context.WithoutCancel(ctx), settings)
	s.logger.Info("database backup completed", "backup_id", backup.ID, "bucket", backup.Bucket, "size", info.Size())
	return nil
}

func hashFile(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind database backup: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("hash database backup: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind database backup: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ListBackups returns recent non-expired runs, newest first.
func (s *Service) ListBackups(ctx context.Context, limit int) ([]Backup, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+backupColumns+` FROM database_backups
		WHERE status <> 'expired' ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list database backups: %w", err)
	}
	defer rows.Close()
	backups := make([]Backup, 0)
	for rows.Next() {
		backup, err := scanBackup(rows, s.credentials.Configured())
		if err != nil {
			return nil, err
		}
		backups = append(backups, backup)
	}
	return backups, rows.Err()
}

// OpenBackup streams a successful object using its snapshotted destination.
func (s *Service) OpenBackup(ctx context.Context, id string) (Backup, io.ReadCloser, error) {
	backup, err := s.getBackup(ctx, id, false)
	if err != nil {
		return Backup{}, nil, err
	}
	if backup.Status != "succeeded" {
		return Backup{}, nil, ErrNotFound
	}
	repo, err := s.repository(settingsFromBackup(backup))
	if err != nil {
		return Backup{}, nil, err
	}
	body, size, err := repo.Open(ctx, backup.ObjectKey)
	if err != nil {
		return Backup{}, nil, err
	}
	if size >= 0 && size != backup.Size {
		body.Close()
		return Backup{}, nil, fmt.Errorf("database backup size is %d, expected %d", size, backup.Size)
	}
	return backup, body, nil
}

// Run keeps scheduled backups enqueued until shutdown.
func (s *Service) Run(ctx context.Context) {
	s.scheduleDue(ctx)
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scheduleDue(ctx)
		}
	}
}

func (s *Service) scheduleDue(ctx context.Context) {
	settings, _, err := s.effectiveSettings(ctx)
	if err != nil {
		s.logger.Error("load backup schedule", "error", err)
		return
	}
	if !settings.Enabled || !s.credentials.Configured() {
		return
	}
	var latest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT max(created_at) FROM database_backups WHERE status <> 'expired'`).Scan(&latest); err != nil {
		s.logger.Error("inspect backup schedule", "error", err)
		return
	}
	if latest != nil && s.now().Before(latest.Add(time.Duration(settings.IntervalHours)*time.Hour)) {
		return
	}
	backup, created, err := s.RequestBackup(ctx, "scheduled")
	if err != nil {
		s.logger.Error("schedule database backup", "error", err)
	} else if created {
		s.logger.Info("scheduled database backup", "backup_id", backup.ID)
	}
}

func (s *Service) expireOldBackups(ctx context.Context, settings Settings) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+backupColumns+` FROM database_backups
		WHERE status = 'succeeded' AND provider = $1 AND endpoint = $2 AND region = $3
		  AND bucket = $4 AND prefix = $5 AND force_path_style = $6
		ORDER BY created_at DESC OFFSET $7`, settings.Provider, settings.Endpoint,
		settings.Region, settings.Bucket, settings.Prefix, settings.ForcePathStyle,
		settings.RetentionCount)
	if err != nil {
		s.logger.Warn("list database backups for retention", "error", err)
		return
	}
	var expired []Backup
	for rows.Next() {
		backup, scanErr := scanBackup(rows, s.credentials.Configured())
		if scanErr != nil {
			s.logger.Warn("read database backup for retention", "error", scanErr)
			break
		}
		expired = append(expired, backup)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("list database backups for retention", "error", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	repo, err := s.repository(settings)
	if err != nil {
		s.logger.Warn("open backup repository for retention", "error", err)
		return
	}
	for _, backup := range expired {
		// The row is expired before the object is deleted, so a run never
		// advertises a download whose object is already gone. The cost of
		// this order is an object left behind when the delete fails, so the
		// row goes back to succeeded and the next retention pass retries it.
		// (internal/attachments/gc.go deletes its row first for the opposite
		// reason: there the row delete is a live re-check against a
		// concurrent download that deduplicated onto the same file. A backup
		// object is referenced by exactly one run, so there is no such race
		// to protect against here.)
		tag, err := s.pool.Exec(ctx, `UPDATE database_backups SET status = 'expired', updated_at = now() WHERE id = $1::uuid AND status = 'succeeded'`, backup.ID)
		if err != nil {
			s.logger.Warn("record expired database backup", "backup_id", backup.ID, "error", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			// Another pass expired this run between the listing and now.
			continue
		}
		if err := repo.Delete(ctx, backup.ObjectKey); err != nil {
			s.logger.Warn("expire database backup", "backup_id", backup.ID, "error", err)
			if _, restoreErr := s.pool.Exec(ctx, `UPDATE database_backups SET status = 'succeeded', updated_at = now() WHERE id = $1::uuid AND status = 'expired'`, backup.ID); restoreErr != nil {
				s.logger.Warn("restore database backup after failed expiry", "backup_id", backup.ID,
					"object_key", backup.ObjectKey, "error", restoreErr)
			}
		}
	}
}

const backupColumns = `id::text, trigger, status, provider, endpoint, region, bucket, prefix,
	force_path_style, retention_count, object_key, size, sha256, error, started_at, completed_at, created_at, updated_at`

func scanBackup(row pgx.Row, credentialsConfigured bool) (Backup, error) {
	var backup Backup
	err := row.Scan(&backup.ID, &backup.Trigger, &backup.Status, &backup.Provider, &backup.Endpoint,
		&backup.Region, &backup.Bucket, &backup.Prefix, &backup.ForcePathStyle,
		&backup.RetentionCount, &backup.ObjectKey, &backup.Size, &backup.SHA256, &backup.Error, &backup.StartedAt,
		&backup.CompletedAt, &backup.CreatedAt, &backup.UpdatedAt)
	backup.DownloadAvailable = err == nil && backup.Status == "succeeded" && credentialsConfigured
	return backup, err
}

func (s *Service) getBackup(ctx context.Context, id string, includeExpired bool) (Backup, error) {
	query := `SELECT ` + backupColumns + ` FROM database_backups WHERE id = $1::uuid`
	if !includeExpired {
		query += ` AND status <> 'expired'`
	}
	backup, err := scanBackup(s.pool.QueryRow(ctx, query, id), s.credentials.Configured())
	if errors.Is(err, pgx.ErrNoRows) {
		return Backup{}, ErrNotFound
	}
	if err != nil {
		return Backup{}, fmt.Errorf("load database backup: %w", err)
	}
	return backup, nil
}

func (s *Service) activeBackup(ctx context.Context) (Backup, error) {
	backup, err := scanBackup(s.pool.QueryRow(ctx, `
		SELECT `+backupColumns+` FROM database_backups
		WHERE status IN ('pending', 'running') ORDER BY created_at DESC LIMIT 1`), s.credentials.Configured())
	if err != nil {
		return Backup{}, fmt.Errorf("load active database backup: %w", err)
	}
	return backup, nil
}

func (s *Service) markRunning(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE database_backups SET status = 'running', started_at = COALESCE(started_at, now()), completed_at = NULL, updated_at = now() WHERE id = $1::uuid`, id)
	return err
}

func (s *Service) markSucceeded(ctx context.Context, id string, size int64, digest string) error {
	_, err := s.pool.Exec(ctx, `UPDATE database_backups SET status = 'succeeded', size = $2, sha256 = $3, error = '', completed_at = now(), updated_at = now() WHERE id = $1::uuid`, id, size, digest)
	return err
}

func (s *Service) markFailed(ctx context.Context, id, message string) error {
	return s.markAttemptFailed(ctx, id, message, true)
}

func (s *Service) markAttemptFailed(ctx context.Context, id, message string, final bool) error {
	message = strings.TrimSpace(message)
	if len(message) > 2000 {
		message = message[:2000]
	}
	status := "pending"
	var completedAt any
	if final {
		status = "failed"
		completedAt = s.now().UTC()
	}
	_, err := s.pool.Exec(ctx, `UPDATE database_backups SET status = $2, error = $3, completed_at = $4, updated_at = now() WHERE id = $1::uuid`, id, status, message, completedAt)
	return err
}

func settingsFromBackup(backup Backup) Settings {
	return Settings{
		Provider:       backup.Provider,
		Endpoint:       backup.Endpoint,
		Region:         backup.Region,
		Bucket:         backup.Bucket,
		Prefix:         backup.Prefix,
		ForcePathStyle: backup.ForcePathStyle,
		RetentionCount: backup.RetentionCount,
	}
}

func randomSuffix() (string, error) {
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate backup object key: %w", err)
	}
	return hex.EncodeToString(value), nil
}
