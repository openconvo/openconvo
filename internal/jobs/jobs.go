// Package jobs is OpenConvo's background job system, backed entirely
// by PostgreSQL — deliberately no Redis or external queue. Workers
// claim jobs atomically with FOR UPDATE SKIP LOCKED, failed jobs retry
// with exponential backoff, and jobs survive process restarts.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job statuses.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Job is one unit of background work.
type Job struct {
	ID          string
	Kind        string
	Payload     json.RawMessage
	Status      string
	Attempts    int
	MaxAttempts int
	DedupeKey   *string
	AvailableAt time.Time
	LastError   string
	CreatedAt   time.Time
}

// UnmarshalPayload decodes the job payload into v.
func (j *Job) UnmarshalPayload(v any) error {
	return json.Unmarshal(j.Payload, v)
}

// Queue enqueues and claims jobs.
type Queue struct {
	pool *pgxpool.Pool
}

// NewQueue creates a Queue backed by the given pool.
func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool}
}

// EnqueueOption customizes an enqueued job.
type EnqueueOption func(*enqueueOptions)

type enqueueOptions struct {
	runAt       time.Time
	maxAttempts int
	dedupeKey   string
}

// WithRunAt delays the job until the given time.
func WithRunAt(t time.Time) EnqueueOption {
	return func(o *enqueueOptions) { o.runAt = t }
}

// WithMaxAttempts overrides the default retry limit.
func WithMaxAttempts(n int) EnqueueOption {
	return func(o *enqueueOptions) { o.maxAttempts = n }
}

// WithDedupeKey suppresses the enqueue when a pending or running job
// with the same key already exists (e.g. one backfill per channel).
func WithDedupeKey(key string) EnqueueOption {
	return func(o *enqueueOptions) { o.dedupeKey = key }
}

// Enqueue adds a job. Returns the job ID, or "" when a dedupe key
// suppressed the enqueue.
func (q *Queue) Enqueue(ctx context.Context, kind string, payload any, opts ...EnqueueOption) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("jobs: kind is required")
	}
	options := enqueueOptions{runAt: time.Now(), maxAttempts: 10}
	for _, opt := range opts {
		opt(&options)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("jobs: marshal payload: %w", err)
	}
	if payload == nil {
		body = []byte("{}")
	}

	var dedupe *string
	if options.dedupeKey != "" {
		dedupe = &options.dedupeKey
	}

	var id string
	err = q.pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, max_attempts, dedupe_key, available_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('pending', 'running')
		DO NOTHING
		RETURNING id::text`,
		kind, body, options.maxAttempts, dedupe, options.runAt.UTC()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // suppressed by dedupe key
	}
	if err != nil {
		return "", fmt.Errorf("jobs: enqueue %s: %w", kind, err)
	}
	return id, nil
}

const jobColumns = `id::text, kind, payload, status, attempts, max_attempts, dedupe_key, available_at, last_error, created_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.Payload, &j.Status, &j.Attempts, &j.MaxAttempts,
		&j.DedupeKey, &j.AvailableAt, &j.LastError, &j.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// claim takes the oldest available job of one of the given kinds. The
// filter is what lets several workers share one queue: a worker must
// never claim a kind it has no handler for, because that job would be
// failed rather than left for its owner.
func (q *Queue) claim(ctx context.Context, kinds []string) (*Job, error) {
	row := q.pool.QueryRow(ctx, `
		UPDATE jobs SET
			status     = 'running',
			attempts   = attempts + 1,
			started_at = now(),
			updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending' AND available_at <= now() AND kind = ANY($1)
			ORDER BY available_at, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+jobColumns, kinds)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: claim: %w", err)
	}
	return job, nil
}

// succeed marks a job completed.
func (q *Queue) succeed(ctx context.Context, id string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = 'succeeded', completed_at = now(), updated_at = now()
		WHERE id = $1::uuid`, id)
	return err
}

// fail records a failed attempt: the job is rescheduled with backoff,
// or marked failed once attempts are exhausted. Both updates only apply
// to a job still "running", so a late failure can never reopen a job
// that has meanwhile completed.
func (q *Queue) fail(ctx context.Context, job *Job, jobErr error) error {
	msg := jobErr.Error()
	if len(msg) > 4000 {
		msg = msg[:4000]
	}
	if job.Attempts >= job.MaxAttempts {
		_, err := q.pool.Exec(ctx, `
			UPDATE jobs SET status = 'failed', last_error = $2, completed_at = now(), updated_at = now()
			WHERE id = $1::uuid AND status = 'running'`, job.ID, msg)
		return err
	}
	delay := Backoff(job.Attempts)
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = 'pending', last_error = $2, available_at = $3, updated_at = now()
		WHERE id = $1::uuid AND status = 'running'`, job.ID, msg, time.Now().UTC().Add(delay))
	return err
}

// release returns a job interrupted by shutdown to "pending", giving
// back the attempt claim consumed: the job was cut short, not tried and
// found wanting. Without this, restarting during a long backfill spends
// the job's retries and eventually fails it for good. available_at is
// left alone so the next process picks the job up immediately.
func (q *Queue) release(ctx context.Context, job *Job) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = 'pending', attempts = greatest(attempts - 1, 0), updated_at = now()
		WHERE id = $1::uuid AND status = 'running'`, job.ID)
	return err
}

// ResetRunning returns jobs of the given kinds stuck in "running" to
// "pending".
//
// The kind filter is what makes this safe with more than one worker.
// OpenConvo runs as a single process with one worker per kind, so a
// worker resetting its own kinds at startup can only be looking at
// leftovers from a previous process: no other worker claims those kinds,
// and its own claims come after this call. Without the filter, a worker
// starting up would requeue jobs another worker had already claimed and
// was executing. If two workers ever share a kind — or several processes
// ever run against one database — this must be replaced with
// lease-based expiry.
func (q *Queue) ResetRunning(ctx context.Context, kinds []string) (int64, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = 'pending', updated_at = now()
		WHERE status = 'running' AND kind = ANY($1)`, kinds)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Get fetches a job by ID.
func (q *Queue) Get(ctx context.Context, id string) (*Job, error) {
	job, err := scanJob(q.pool.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("jobs: job %s not found", id)
	}
	return job, err
}

// Backoff returns the retry delay after the given attempt number
// (1-based): exponential with a cap, so transient Discord or network
// failures back off politely. Attempts below 1 are treated as the first
// attempt rather than panicking on a negative shift.
func Backoff(attempt int) time.Duration {
	const (
		base = 3 * time.Second
		cap  = 10 * time.Minute
	)
	if attempt < 1 {
		attempt = 1
	}
	d := base << (attempt - 1)
	if d > cap || d <= 0 {
		return cap
	}
	return d
}
