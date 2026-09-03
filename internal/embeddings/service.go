package embeddings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/jobs"
)

const (
	sweepInterval   = 6 * time.Hour
	sweepBatchSize  = 64
	jobMaxAttempts  = 10
	connectionCheck = "OpenConvo embedding connection check."
)

// blankChars spells out, as a PostgreSQL escape-string body, every rune Go's
// strings.TrimSpace strips. PostgreSQL's single-argument btrim strips only
// U+0020, so a message holding just a newline, a tab or a U+00A0 would satisfy
// an SQL "not blank" filter and then be refused as empty input by the provider
// client: a candidate that looks eligible forever, and can never be embedded.
// Vertical tab is spelled as a code point because PostgreSQL escape strings do
// not understand \v and would trim a literal "v".
const blankChars = ` \t\n\u000B\f\r\u0085\u00A0\u1680` +
	`\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A` +
	`\u2028\u2029\u202F\u205F\u3000`

// hasContent renders the shared "this message is worth embedding" predicate.
// Every caller passes a column name that is a literal in this file.
func hasContent(column string) string {
	return column + ` IS NOT NULL AND btrim(` + column + `, E'` + blankChars + `') <> ''`
}

type Options struct {
	Defaults Settings
	APIKey   string
}

// Service owns the optional embedding configuration and derived indexing
// pipeline. Canonical ingestion only asks it to schedule message IDs.
type Service struct {
	pool     *pgxpool.Pool
	queue    *jobs.Queue
	defaults Settings
	embedder embedder
	apiKey   string
	logger   *slog.Logger
	enabled  atomic.Bool
}

func New(pool *pgxpool.Pool, queue *jobs.Queue, opts Options, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	defaults, err := normalizeSettings(opts.Defaults)
	if err != nil {
		defaults = Preset(false)
	}
	var provider embedder
	if strings.TrimSpace(opts.APIKey) != "" {
		provider = newOpenAIClient(opts.APIKey)
	}
	return &Service{
		pool:     pool,
		queue:    queue,
		defaults: defaults,
		embedder: provider,
		apiKey:   strings.TrimSpace(opts.APIKey),
		logger:   logger.With("component", "embeddings"),
	}
}

// Initialize loads the persisted opt-in before ingestion begins scheduling
// work. A missing credential degrades only this derived subsystem.
func (s *Service) Initialize(ctx context.Context) error {
	settings, _, err := s.effectiveSettings(ctx)
	if err != nil {
		return err
	}
	s.enabled.Store(settings.Enabled && s.configured())
	if settings.Enabled && !s.configured() {
		s.logger.Warn("embeddings enabled but OPENAI_API_KEY is not configured; indexing is paused")
	}
	if settings.Enabled && s.configured() {
		if _, err := s.ensureGeneration(ctx); err != nil {
			return err
		}
		s.scheduleSweep(ctx)
	}
	return nil
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) {
	worker.Register(JobMessage, s.handleMessage)
	worker.Register(JobSweep, s.handleSweep)
}

// Run periodically reconciles missing vectors. This heals enqueue failures,
// interrupted backfills, and any derived rows discarded after edits.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scheduleSweep(ctx)
		}
	}
}

// ScheduleMessage is intentionally best-effort. The canonical message is
// already committed; a periodic sweep recovers any missed derived work.
func (s *Service) ScheduleMessage(ctx context.Context, messageID string) {
	if !s.enabled.Load() || messageID == "" {
		return
	}
	_, err := s.queue.Enqueue(ctx, JobMessage, messagePayload{MessageID: messageID},
		jobs.WithDedupeKey(JobMessage+":"+messageID), jobs.WithMaxAttempts(jobMaxAttempts))
	if err != nil {
		s.logger.Warn("schedule message embedding", "message_id", messageID, "error", err)
	}
}

func (s *Service) scheduleSweep(ctx context.Context) {
	if !s.enabled.Load() {
		return
	}
	if _, err := s.queue.Enqueue(ctx, JobSweep, nil,
		jobs.WithDedupeKey(JobSweep), jobs.WithMaxAttempts(jobMaxAttempts)); err != nil {
		s.logger.Warn("schedule embedding sweep", "error", err)
	}
}

func (s *Service) GetSettings(ctx context.Context) (SettingsView, error) {
	settings, source, err := s.effectiveSettings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	view := SettingsView{
		Settings:              settings,
		CredentialsConfigured: s.configured(),
		Source:                source,
	}
	generation, found, err := s.generation(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	if found {
		view.GenerationStatus = generation.Status
		if err := s.pool.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM derived.message_embeddings WHERE generation_id=$1::uuid),
				(SELECT count(*) FROM messages WHERE deleted_at IS NULL AND `+hasContent("content")+`)`,
			generation.ID).Scan(&view.EmbeddedMessages, &view.EligibleMessages); err != nil {
			return SettingsView{}, fmt.Errorf("embedding stats: %w", err)
		}
	} else if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM messages WHERE deleted_at IS NULL AND `+hasContent("content")).
		Scan(&view.EligibleMessages); err != nil {
		return SettingsView{}, fmt.Errorf("embedding stats: %w", err)
	}
	return view, nil
}

// SaveSettings persists only the non-secret opt-in. The API key remains in the
// environment. Enabling performs a harmless fixed-text request before any
// archived content is sent.
func (s *Service) SaveSettings(ctx context.Context, input Settings) (SettingsView, error) {
	settings, err := normalizeSettings(input)
	if err != nil {
		return SettingsView{}, err
	}
	if settings.Enabled {
		if !s.configured() {
			return SettingsView{}, fmt.Errorf("%w: set OPENAI_API_KEY and restart OpenConvo", ErrNotConfigured)
		}
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err = s.embedder.Embed(checkCtx, []string{connectionCheck})
		cancel()
		if err != nil {
			return SettingsView{}, fmt.Errorf("verify embedding provider: %w", err)
		}
		if _, err := s.ensureGeneration(ctx); err != nil {
			return SettingsView{}, err
		}
	}
	body, err := json.Marshal(settings)
	if err != nil {
		return SettingsView{}, fmt.Errorf("encode embedding settings: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, settingsKey, body); err != nil {
		return SettingsView{}, fmt.Errorf("save embedding settings: %w", err)
	}
	s.enabled.Store(settings.Enabled && s.configured())
	if settings.Enabled {
		s.scheduleSweep(context.WithoutCancel(ctx))
	}
	return s.GetSettings(ctx)
}

func (s *Service) configured() bool {
	return s.embedder != nil && s.apiKey != ""
}

func (s *Service) effectiveSettings(ctx context.Context) (Settings, string, error) {
	var body []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, settingsKey).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		settings, normalizeErr := normalizeSettings(s.defaults)
		if normalizeErr != nil {
			return Settings{}, "", normalizeErr
		}
		return settings, "environment", nil
	}
	if err != nil {
		return Settings{}, "", fmt.Errorf("load embedding settings: %w", err)
	}
	var settings Settings
	if err := json.Unmarshal(body, &settings); err != nil {
		return Settings{}, "", fmt.Errorf("decode embedding settings: %w", err)
	}
	settings, err = normalizeSettings(settings)
	if err != nil {
		return Settings{}, "", fmt.Errorf("stored embedding settings: %w", err)
	}
	return settings, "dashboard", nil
}

type generationRow struct {
	ID     string
	Status string
}

func (s *Service) generation(ctx context.Context) (generationRow, bool, error) {
	var row generationRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, status FROM derived.embedding_generations
		WHERE provider=$1 AND model=$2 AND dimensions=$3 AND input_version=$4`,
		ProviderOpenAI, ModelSmall, Dimensions, InputVersion).Scan(&row.ID, &row.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return generationRow{}, false, nil
	}
	if err != nil {
		return generationRow{}, false, fmt.Errorf("load embedding generation: %w", err)
	}
	return row, true, nil
}

func (s *Service) ensureGeneration(ctx context.Context) (generationRow, error) {
	var row generationRow
	err := s.pool.QueryRow(ctx, `
		INSERT INTO derived.embedding_generations (provider, model, dimensions, input_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, model, dimensions, input_version)
		DO UPDATE SET provider=EXCLUDED.provider
		RETURNING id::text, status`, ProviderOpenAI, ModelSmall, Dimensions, InputVersion).
		Scan(&row.ID, &row.Status)
	if err != nil {
		return generationRow{}, fmt.Errorf("ensure embedding generation: %w", err)
	}
	return row, nil
}

type messagePayload struct {
	MessageID string `json:"message_id"`
}

func (s *Service) handleMessage(ctx context.Context, job *jobs.Job) error {
	if !s.enabled.Load() || !s.configured() {
		return nil
	}
	var payload messagePayload
	if err := job.UnmarshalPayload(&payload); err != nil {
		return fmt.Errorf("decode embedding message job: %w", err)
	}
	if payload.MessageID == "" {
		return fmt.Errorf("decode embedding message job: message_id is required")
	}
	generation, err := s.ensureGeneration(ctx)
	if err != nil {
		return err
	}
	row, found, err := s.loadMessage(ctx, generation.ID, payload.MessageID)
	if err != nil || !found {
		return err
	}
	result, err := s.embedRows(ctx, generation.ID, []messageRow{row})
	if err != nil {
		// The provider will refuse this input again however often the job is
		// retried, so record it and let the job succeed instead of burning
		// every attempt on it.
		if errors.Is(err, ErrRejectedInput) {
			s.logger.Warn("embedding provider rejected message", "message_id", row.ID, "error", err)
			return nil
		}
		return err
	}
	if result.stored == 0 && result.eligible > 0 {
		return fmt.Errorf("message changed while its embedding was generated")
	}
	return nil
}

func (s *Service) handleSweep(ctx context.Context, _ *jobs.Job) error {
	if !s.enabled.Load() || !s.configured() {
		return nil
	}
	generation, err := s.ensureGeneration(ctx)
	if err != nil {
		return err
	}
	// Rows this generation cannot embed — blank once trimmed, or refused
	// outright by the provider — are held aside for the rest of the run.
	// loadMissing always returns the lowest-id rows still missing a vector, so
	// without this one permanently rejected row would come back in every batch,
	// every retry and every later sweep, and nothing behind it would ever be
	// embedded. Nothing is persisted: the next sweep offers these rows once
	// more, so an edited or repaired message heals itself. The dashboard still
	// counts them as eligible, so a stubborn row shows up as a small permanent
	// gap between the embedded and eligible totals rather than a stalled index.
	var skipped []string
	for {
		// Consent can be withdrawn while a first backfill is still running.
		// Stop sending content at the next batch boundary; the operator asked
		// for exactly this, so the job has not failed.
		if !s.enabled.Load() {
			s.logger.Info("embedding sweep stopped because embeddings were disabled")
			return nil
		}
		rows, err := s.loadMissing(ctx, generation.ID, skipped, sweepBatchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		result, err := s.embedBatch(ctx, generation.ID, rows)
		if err != nil {
			return err
		}
		skipped = append(skipped, result.skipped...)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if len(skipped) > 0 {
		s.logger.Warn("embedding sweep finished with unembeddable messages", "count", len(skipped))
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("activate embedding generation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		UPDATE derived.embedding_generations SET status='retired'
		WHERE status='active' AND id <> $1::uuid`, generation.ID); err != nil {
		return fmt.Errorf("retire prior embedding generation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE derived.embedding_generations SET status='active', activated_at=COALESCE(activated_at, now())
		WHERE id=$1::uuid`, generation.ID); err != nil {
		return fmt.Errorf("activate embedding generation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("activate embedding generation: %w", err)
	}
	return nil
}

type messageRow struct {
	ID           string
	Content      string
	ExistingHash *string
}

func (s *Service) loadMessage(ctx context.Context, generationID, messageID string) (messageRow, bool, error) {
	var row messageRow
	err := s.pool.QueryRow(ctx, `
		SELECT m.id::text, m.content, e.content_hash
		FROM messages m
		LEFT JOIN derived.message_embeddings e
		  ON e.message_id=m.id AND e.generation_id=$1::uuid
		WHERE m.id=$2::uuid AND m.deleted_at IS NULL AND `+hasContent("m.content"),
		generationID, messageID).Scan(&row.ID, &row.Content, &row.ExistingHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return messageRow{}, false, nil
	}
	if err != nil {
		return messageRow{}, false, fmt.Errorf("load message for embedding: %w", err)
	}
	if row.ExistingHash != nil && *row.ExistingHash == contentHash(row.Content) {
		return messageRow{}, false, nil
	}
	return row, true, nil
}

// loadMissing returns the lowest-id candidates still missing a vector, minus
// the messages this run already established cannot be embedded.
func (s *Service) loadMissing(ctx context.Context, generationID string, skip []string, limit int) ([]messageRow, error) {
	if skip == nil {
		// A nil slice reaches PostgreSQL as NULL, and "NOT (id = ANY(NULL))"
		// is NULL, which would silently discard every candidate.
		skip = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.id::text, m.content, e.content_hash
		FROM messages m
		LEFT JOIN derived.message_embeddings e
		  ON e.message_id=m.id AND e.generation_id=$1::uuid
		WHERE m.deleted_at IS NULL AND `+hasContent("m.content")+`
		  AND e.message_id IS NULL
		  AND NOT (m.id = ANY($3::uuid[]))
		ORDER BY m.id
		LIMIT $2`, generationID, limit, skip)
	if err != nil {
		return nil, fmt.Errorf("list messages missing embeddings: %w", err)
	}
	defer rows.Close()
	out := make([]messageRow, 0, limit)
	for rows.Next() {
		var row messageRow
		if err := rows.Scan(&row.ID, &row.Content, &row.ExistingHash); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// batchResult reports what one batch achieved: how many vectors were
// committed, how many inputs were still eligible after provider latency, and
// which messages this generation cannot embed at all.
type batchResult struct {
	stored   int
	eligible int
	skipped  []string
}

// embedBatch isolates inputs the provider refuses. One request is
// all-or-nothing, so a single permanently rejected message would otherwise
// fail every batch it appears in — and because candidates are read in id
// order, that is every batch for the rest of the archive. Halving the batch
// finds the offending rows in a handful of extra requests and lets everything
// else through. Transient failures (rate limits, server errors, network) are
// returned unchanged so the job retries them.
func (s *Service) embedBatch(ctx context.Context, generationID string, rows []messageRow) (batchResult, error) {
	result, err := s.embedRows(ctx, generationID, rows)
	if err == nil || !errors.Is(err, ErrRejectedInput) {
		return result, err
	}
	if len(rows) == 1 {
		s.logger.Warn("embedding provider rejected message", "message_id", rows[0].ID, "error", err)
		return batchResult{skipped: []string{rows[0].ID}}, nil
	}
	half := len(rows) / 2
	first, err := s.embedBatch(ctx, generationID, rows[:half])
	if err != nil {
		return batchResult{}, err
	}
	second, err := s.embedBatch(ctx, generationID, rows[half:])
	if err != nil {
		return batchResult{}, err
	}
	return batchResult{
		stored:   first.stored + second.stored,
		eligible: first.eligible + second.eligible,
		skipped:  append(first.skipped, second.skipped...),
	}, nil
}

// embedRows sends one batch and stores the vectors it gets back. A changed
// message is left missing so the caller or next sweep regenerates it from
// current canonical content.
func (s *Service) embedRows(ctx context.Context, generationID string, rows []messageRow) (batchResult, error) {
	var result batchResult
	candidates := make([]messageRow, 0, len(rows))
	for _, row := range rows {
		// Go's definition of whitespace is the authoritative one: the provider
		// client refuses any input that trims to empty. A row the SQL filter
		// let through anyway is held aside rather than sent and refused.
		if strings.TrimSpace(row.Content) == "" {
			result.skipped = append(result.skipped, row.ID)
			continue
		}
		candidates = append(candidates, row)
	}
	// Consent may have been withdrawn since these rows were read.
	if !s.enabled.Load() || len(candidates) == 0 {
		return result, nil
	}
	candidates, err := s.pruneDeleted(ctx, candidates)
	if err != nil {
		return result, err
	}
	if len(candidates) == 0 {
		return result, nil
	}
	input := make([]string, len(candidates))
	for i := range candidates {
		input[i] = candidates[i].Content
	}
	vectors, err := s.embedder.Embed(ctx, input)
	if err != nil {
		return result, err
	}
	if len(vectors) != len(candidates) {
		return result, fmt.Errorf("embedding provider returned %d vectors for %d messages", len(vectors), len(candidates))
	}
	for i, row := range candidates {
		literal, err := vectorLiteral(vectors[i])
		if err != nil {
			return result, err
		}
		committed, stillEligible, err := s.storeEmbedding(ctx, generationID, row, literal)
		if err != nil {
			return result, err
		}
		if stillEligible {
			result.eligible++
		}
		if committed {
			result.stored++
		}
	}
	return result, nil
}

// pruneDeleted drops messages tombstoned or blanked since the candidate rows
// were read. It narrows, but cannot close, the window in which the content of
// a just-deleted message is sent to the provider: honouring a tombstone that
// lands while the request is in flight would need a transaction spanning a
// network call. storeEmbedding re-checks under FOR SHARE, so no vector is ever
// stored for a deleted message; what remains is that its text may already have
// left the machine.
func (s *Service) pruneDeleted(ctx context.Context, rows []messageRow) ([]messageRow, error) {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	live := make(map[string]struct{}, len(ids))
	found, err := s.pool.Query(ctx, `
		SELECT id::text FROM messages
		WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL AND `+hasContent("content"), ids)
	if err != nil {
		return nil, fmt.Errorf("re-check messages before embedding: %w", err)
	}
	defer found.Close()
	for found.Next() {
		var id string
		if err := found.Scan(&id); err != nil {
			return nil, err
		}
		live[id] = struct{}{}
	}
	if err := found.Err(); err != nil {
		return nil, fmt.Errorf("re-check messages before embedding: %w", err)
	}
	if len(live) == len(rows) {
		return rows, nil
	}
	kept := make([]messageRow, 0, len(live))
	for _, row := range rows {
		if _, ok := live[row.ID]; ok {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

func (s *Service) storeEmbedding(ctx context.Context, generationID string, row messageRow, literal string) (bool, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var current *string
	var deletedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT content, deleted_at FROM messages WHERE id=$1::uuid FOR SHARE`, row.ID).
		Scan(&current, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if deletedAt != nil || current == nil || strings.TrimSpace(*current) == "" {
		return false, false, nil
	}
	if *current != row.Content {
		return false, true, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO derived.message_embeddings (generation_id, message_id, content_hash, embedding)
		VALUES ($1::uuid, $2::uuid, $3, $4::vector)
		ON CONFLICT (generation_id, message_id) DO UPDATE SET
			content_hash=EXCLUDED.content_hash, embedding=EXCLUDED.embedding, updated_at=now()`,
		generationID, row.ID, contentHash(row.Content), literal); err != nil {
		return false, true, fmt.Errorf("store message embedding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, true, err
	}
	return true, true, nil
}

func contentHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func vectorLiteral(values []float32) (string, error) {
	if len(values) != Dimensions {
		return "", fmt.Errorf("embedding has %d dimensions, expected %d", len(values), Dimensions)
	}
	buf := make([]byte, 0, Dimensions*12)
	buf = append(buf, '[')
	for i, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return "", fmt.Errorf("embedding dimension %d is not finite", i)
		}
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendFloat(buf, float64(value), 'g', -1, 32)
	}
	buf = append(buf, ']')
	return string(buf), nil
}
