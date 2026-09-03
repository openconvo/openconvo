package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists the canonical archive in PostgreSQL.
//
// Write semantics, in order of importance:
//
//  1. Idempotent: every upsert can be applied any number of times
//     (duplicate Gateway events, overlapping backfills, reconciliation).
//  2. Defensive: partial update events never erase previously known
//     fields (message update payloads can be partial).
//  3. Deletion-safe: tombstoned messages are never resurrected by stale
//     events, and every deletion is recorded in the deletion ledger for
//     audit and optional replay after restoring an older snapshot.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a Store backed by the given connection pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ErrNotFound is returned when a referenced row does not exist.
var ErrNotFound = errors.New("archive: not found")

// ---------------------------------------------------------------------------
// Communities

// CommunityUpsert is the input for UpsertCommunity. Community payloads
// from sources are complete objects, so fields overwrite existing values.
type CommunityUpsert struct {
	Source      string
	ExternalID  string
	Name        string
	Description string
	IconURL     string
	RawPayload  json.RawMessage
}

const communityColumns = `id::text, source, external_id, name, description, icon_url, raw_payload, created_at, updated_at`

func scanCommunity(row pgx.Row) (Community, error) {
	var c Community
	err := row.Scan(&c.ID, &c.Source, &c.ExternalID, &c.Name, &c.Description,
		&c.IconURL, &c.RawPayload, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// UpsertCommunity inserts or updates a community by (source, external_id).
func (s *Store) UpsertCommunity(ctx context.Context, in CommunityUpsert) (Community, error) {
	if in.Source == "" || in.ExternalID == "" {
		return Community{}, fmt.Errorf("upsert community: source and external_id are required")
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO communities (source, external_id, name, description, icon_url, raw_payload)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, '{}'::jsonb))
		ON CONFLICT (source, external_id) DO UPDATE SET
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			icon_url    = EXCLUDED.icon_url,
			raw_payload = CASE WHEN EXCLUDED.raw_payload = '{}'::jsonb
			                   THEN communities.raw_payload
			                   ELSE EXCLUDED.raw_payload END,
			updated_at  = now()
		RETURNING `+communityColumns,
		in.Source, in.ExternalID, in.Name, in.Description, in.IconURL, in.RawPayload)
	return scanCommunity(row)
}

// ---------------------------------------------------------------------------
// Channels

// ChannelUpsert is the input for UpsertChannel. ArchiveEnabled is
// deliberately absent: whether a channel is archived is an operator
// decision made through SetChannelArchiveEnabled, never implied by sync.
type ChannelUpsert struct {
	CommunityID     string
	ExternalID      string
	ParentChannelID *string
	Kind            string
	Name            string
	Topic           string
	Position        int
	IsPrivate       bool
	IsArchived      bool
	SourceCreatedAt *time.Time
	RawPayload      json.RawMessage
}

const channelColumns = `id::text, community_id::text, external_id, parent_channel_id::text, kind, name, topic,
	position, is_private, is_archived, archive_enabled, source_created_at, raw_payload, created_at, updated_at`

// prefixedChannelColumns qualifies channelColumns with a table alias, so
// the same column list works in joined queries. The casts survive the
// prefixing: "id::text" becomes "ch.id::text".
func prefixedChannelColumns(alias string) string {
	parts := strings.Split(channelColumns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func scanChannel(row pgx.Row) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.CommunityID, &c.ExternalID, &c.ParentChannelID, &c.Kind,
		&c.Name, &c.Topic, &c.Position, &c.IsPrivate, &c.IsArchived, &c.ArchiveEnabled,
		&c.SourceCreatedAt, &c.RawPayload, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// UpsertChannel inserts or updates a channel by (community_id, external_id).
func (s *Store) UpsertChannel(ctx context.Context, in ChannelUpsert) (Channel, error) {
	if in.CommunityID == "" || in.ExternalID == "" {
		return Channel{}, fmt.Errorf("upsert channel: community_id and external_id are required")
	}
	kind := in.Kind
	if kind == "" {
		kind = "text"
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO channels (community_id, external_id, parent_channel_id, kind, name, topic,
		                      position, is_private, is_archived, source_created_at, raw_payload)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, '{}'::jsonb))
		ON CONFLICT (community_id, external_id) DO UPDATE SET
			parent_channel_id = COALESCE(EXCLUDED.parent_channel_id, channels.parent_channel_id),
			kind              = EXCLUDED.kind,
			name              = EXCLUDED.name,
			topic             = EXCLUDED.topic,
			position          = EXCLUDED.position,
			is_private        = EXCLUDED.is_private,
			is_archived       = EXCLUDED.is_archived,
			source_created_at = COALESCE(EXCLUDED.source_created_at, channels.source_created_at),
			raw_payload       = CASE WHEN EXCLUDED.raw_payload = '{}'::jsonb
			                         THEN channels.raw_payload
			                         ELSE EXCLUDED.raw_payload END,
			updated_at        = now()
		RETURNING `+channelColumns,
		in.CommunityID, in.ExternalID, in.ParentChannelID, kind, in.Name, in.Topic,
		in.Position, in.IsPrivate, in.IsArchived, in.SourceCreatedAt, in.RawPayload)
	return scanChannel(row)
}

// SetChannelArchiveEnabled records the operator's decision to archive
// (or stop archiving) a channel.
func (s *Store) SetChannelArchiveEnabled(ctx context.Context, channelID string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE channels SET archive_enabled = $2, updated_at = now() WHERE id = $1::uuid`,
		channelID, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Actors

// ActorUpsert is the input for UpsertActor.
type ActorUpsert struct {
	Source      string
	ExternalID  string
	Username    string
	DisplayName string
	AvatarURL   string
	IsBot       bool
	RawPayload  json.RawMessage
}

const actorColumns = `id::text, source, external_id, username, display_name, avatar_url, is_bot, raw_payload, created_at, updated_at`

func scanActor(row pgx.Row) (Actor, error) {
	var a Actor
	err := row.Scan(&a.ID, &a.Source, &a.ExternalID, &a.Username, &a.DisplayName,
		&a.AvatarURL, &a.IsBot, &a.RawPayload, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// UpsertActor inserts or updates an actor by (source, external_id).
func (s *Store) UpsertActor(ctx context.Context, in ActorUpsert) (Actor, error) {
	if in.Source == "" || in.ExternalID == "" {
		return Actor{}, fmt.Errorf("upsert actor: source and external_id are required")
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO actors (source, external_id, username, display_name, avatar_url, is_bot, raw_payload)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, '{}'::jsonb))
		ON CONFLICT (source, external_id) DO UPDATE SET
			username     = CASE WHEN EXCLUDED.username <> '' THEN EXCLUDED.username ELSE actors.username END,
			display_name = CASE WHEN EXCLUDED.display_name <> '' THEN EXCLUDED.display_name ELSE actors.display_name END,
			avatar_url   = EXCLUDED.avatar_url,
			is_bot       = EXCLUDED.is_bot,
			raw_payload  = CASE WHEN EXCLUDED.raw_payload = '{}'::jsonb
			                    THEN actors.raw_payload
			                    ELSE EXCLUDED.raw_payload END,
			updated_at   = now()
		RETURNING `+actorColumns,
		in.Source, in.ExternalID, in.Username, in.DisplayName, in.AvatarURL, in.IsBot, in.RawPayload)
	return scanActor(row)
}

// ---------------------------------------------------------------------------
// Messages

// MessageUpsert is the input for UpsertMessage.
//
// Optional pointer fields mean "not provided by this event": a nil
// Content on an update never erases existing content, because source
// platforms (Discord included) may send partial update payloads.
// Kind is not a pointer because "default" doubles as the fallback for
// events that omit the message type, so it never overwrites a kind an
// earlier event established.
type MessageUpsert struct {
	ChannelID         string
	ActorID           *string
	ExternalID        string
	Kind              string
	Content           *string
	ReplyToExternalID *string
	SourceCreatedAt   time.Time
	SourceUpdatedAt   *time.Time
	RawPayload        json.RawMessage
}

const messageColumns = `id::text, channel_id::text, actor_id::text, external_id, kind, content,
	reply_to_message_id::text, reply_to_external_id, source_created_at, source_updated_at,
	deleted_at, raw_payload, created_at, updated_at`

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ChannelID, &m.ActorID, &m.ExternalID, &m.Kind, &m.Content,
		&m.ReplyToMessageID, &m.ReplyToExternalID, &m.SourceCreatedAt, &m.SourceUpdatedAt,
		&m.DeletedAt, &m.RawPayload, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

// UpsertMessage inserts or updates a message by (channel_id, external_id).
//
// A tombstoned (deleted) message is never modified: stale create/update
// events arriving after a deletion return the tombstone unchanged.
// Reply references are resolved to archived message IDs when the
// referenced message is already present; otherwise the external
// reference is kept for later resolution.
//
// Updates never erase known fields or allow an older source snapshot to
// replace a newer edit. Beyond the COALESCEd columns, a kind of "default"
// leaves an established kind alone (it is the fallback for events without a
// message type), and the raw payload is merged key by key rather than replaced,
// so an event carrying only what changed keeps the rest of the archived source
// object.
func (s *Store) UpsertMessage(ctx context.Context, in MessageUpsert) (Message, error) {
	if in.ChannelID == "" || in.ExternalID == "" {
		return Message{}, fmt.Errorf("upsert message: channel_id and external_id are required")
	}
	if in.SourceCreatedAt.IsZero() {
		return Message{}, fmt.Errorf("upsert message: source_created_at is required")
	}
	kind := in.Kind
	if kind == "" {
		kind = "default"
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO messages (channel_id, actor_id, external_id, kind, content,
		                      reply_to_message_id, reply_to_external_id,
		                      source_created_at, source_updated_at, raw_payload)
		SELECT $1::uuid, $2::uuid, $3, $4, $5,
		       (SELECT r.id FROM messages r WHERE r.channel_id = $1::uuid AND r.external_id = $6),
		       $6, $7, $8, COALESCE($9, '{}'::jsonb)
		WHERE NOT EXISTS (
			SELECT 1
			FROM deletion_ledger d
			JOIN channels ch ON ch.id = $1::uuid
			JOIN communities co ON co.id = ch.community_id
			WHERE d.source = co.source
			  AND d.object_type = '`+ObjectTypeMessage+`'
			  AND d.external_id = $3
		)
		ON CONFLICT (channel_id, external_id) DO UPDATE SET
			actor_id             = COALESCE(EXCLUDED.actor_id, messages.actor_id),
			kind                 = CASE WHEN EXCLUDED.kind <> 'default'
			                            THEN EXCLUDED.kind
			                            ELSE messages.kind END,
			content              = COALESCE(EXCLUDED.content, messages.content),
			reply_to_message_id  = COALESCE(EXCLUDED.reply_to_message_id, messages.reply_to_message_id),
			reply_to_external_id = COALESCE(EXCLUDED.reply_to_external_id, messages.reply_to_external_id),
			source_updated_at    = COALESCE(EXCLUDED.source_updated_at, messages.source_updated_at),
			raw_payload          = messages.raw_payload || EXCLUDED.raw_payload,
			updated_at           = now()
		WHERE messages.deleted_at IS NULL
		  AND (
			(messages.source_updated_at IS NULL AND EXCLUDED.source_updated_at IS NULL)
			OR (EXCLUDED.source_updated_at IS NOT NULL AND
			    (messages.source_updated_at IS NULL OR EXCLUDED.source_updated_at >= messages.source_updated_at))
		  )
		RETURNING `+messageColumns,
		in.ChannelID, in.ActorID, in.ExternalID, kind, in.Content,
		in.ReplyToExternalID, in.SourceCreatedAt, in.SourceUpdatedAt, in.RawPayload)

	msg, err := scanMessage(row)
	if err == nil {
		return msg, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Message{}, err
	}

	// No row returned: the message exists but is tombstoned or newer than this
	// source snapshot. Return the canonical row so callers observe the durable
	// state rather than treating the guarded no-op as an error.
	existing, found, err := s.GetMessageByExternalID(ctx, in.ChannelID, in.ExternalID)
	if err != nil {
		return Message{}, err
	}
	if found {
		return existing, nil
	}
	// A replayed deletion ledger can precede historical backfill without a
	// canonical tombstone row. Return a logical tombstone so ingestion skips
	// attachments and reactions without recreating its content.
	var deletedAt time.Time
	err = s.pool.QueryRow(ctx, `
			SELECT d.deleted_at
			FROM deletion_ledger d
			JOIN channels ch ON ch.id = $1::uuid
			JOIN communities co ON co.id = ch.community_id
			WHERE d.source = co.source
			  AND d.object_type = $2
			  AND d.external_id = $3
			ORDER BY d.deleted_at
			LIMIT 1`, in.ChannelID, ObjectTypeMessage, in.ExternalID).Scan(&deletedAt)
	if err == nil {
		return Message{
			ChannelID:       in.ChannelID,
			ExternalID:      in.ExternalID,
			Kind:            kind,
			SourceCreatedAt: in.SourceCreatedAt,
			DeletedAt:       &deletedAt,
			RawPayload:      json.RawMessage(`{}`),
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Message{}, err
	}
	return Message{}, fmt.Errorf("upsert message %s: no row updated and none found", in.ExternalID)
}

// GetMessageByExternalID fetches a message by its source identity.
func (s *Store) GetMessageByExternalID(ctx context.Context, channelID, externalID string) (Message, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE channel_id = $1::uuid AND external_id = $2`,
		channelID, externalID)
	m, err := scanMessage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, err
	}
	return m, true, nil
}

// MarkMessageDeleted tombstones a message: content and raw payload are
// scrubbed, dependent records (attachments, reactions, bookmarks) are
// removed, and the deletion is recorded in the deletion ledger.
//
// The ledger entry is written even when the message was never archived,
// so deletions observed during a partial import still hold after later
// backfills or restores. An unseen deletion creates a content-free canonical
// tombstone using the source creation time; returns false when no message row
// existed before this call.
func (s *Store) MarkMessageDeleted(ctx context.Context, source, channelID, externalID string, sourceCreatedAt time.Time) (bool, error) {
	if source == "" || channelID == "" || externalID == "" {
		return false, fmt.Errorf("mark message deleted: source, channel_id and external_id are required")
	}
	if sourceCreatedAt.IsZero() {
		return false, fmt.Errorf("mark message deleted: source_created_at is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var messageID string
	var existed bool
	err = tx.QueryRow(ctx, `
		WITH prior AS MATERIALIZED (
			SELECT id FROM messages WHERE channel_id = $1::uuid AND external_id = $2
		), tombstone AS (
			INSERT INTO messages (channel_id, external_id, source_created_at, deleted_at)
			VALUES ($1::uuid, $2, $3, now())
			ON CONFLICT (channel_id, external_id) DO UPDATE SET
				content     = NULL,
				deleted_at  = COALESCE(messages.deleted_at, now()),
				raw_payload = '{}'::jsonb,
				updated_at  = now()
			RETURNING id
		)
		SELECT tombstone.id::text, EXISTS (SELECT 1 FROM prior)
		FROM tombstone`, channelID, externalID, sourceCreatedAt).Scan(&messageID, &existed)
	if err != nil {
		return false, err
	}

	for _, stmt := range []string{
		`DELETE FROM attachments WHERE message_id = $1::uuid`,
		`DELETE FROM message_reactions WHERE message_id = $1::uuid`,
		`DELETE FROM bookmarks WHERE message_id = $1::uuid`,
	} {
		if _, err := tx.Exec(ctx, stmt, messageID); err != nil {
			return false, err
		}
	}

	// Orphaned blobs (no attachment references left) are removed by the
	// storage cleanup job, which also deletes the physical file.
	if _, err := tx.Exec(ctx, `
		INSERT INTO deletion_ledger (source, object_type, external_id)
		VALUES ($1, $2, $3)`,
		source, ObjectTypeMessage, externalID); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return existed, nil
}

// ---------------------------------------------------------------------------
// Reactions

// SetReaction records an absolute reaction count (used by backfill and
// reconciliation, which observe totals).
func (s *Store) SetReaction(ctx context.Context, messageID, emojiKey, emojiName string, count int, raw json.RawMessage) error {
	if count <= 0 {
		_, err := s.pool.Exec(ctx,
			`DELETE FROM message_reactions WHERE message_id = $1::uuid AND emoji_key = $2`,
			messageID, emojiKey)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO message_reactions (message_id, emoji_key, emoji_name, count, raw_payload)
		VALUES ($1::uuid, $2, $3, $4, COALESCE($5, '{}'::jsonb))
		ON CONFLICT (message_id, emoji_key) DO UPDATE SET
			emoji_name  = EXCLUDED.emoji_name,
			count       = EXCLUDED.count,
			raw_payload = CASE WHEN EXCLUDED.raw_payload = '{}'::jsonb
			                   THEN message_reactions.raw_payload
			                   ELSE EXCLUDED.raw_payload END,
			updated_at  = now()`,
		messageID, emojiKey, emojiName, count, raw)
	return err
}

// AdjustReaction applies a live add/remove delta. Counts never go below
// zero, and rows at zero are removed.
func (s *Store) AdjustReaction(ctx context.Context, messageID, emojiKey, emojiName string, delta int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO message_reactions (message_id, emoji_key, emoji_name, count)
		VALUES ($1::uuid, $2, $3, GREATEST($4, 0))
		ON CONFLICT (message_id, emoji_key) DO UPDATE SET
			count      = GREATEST(message_reactions.count + $4, 0),
			emoji_name = CASE WHEN EXCLUDED.emoji_name <> '' THEN EXCLUDED.emoji_name
			                  ELSE message_reactions.emoji_name END,
			updated_at = now()`,
		messageID, emojiKey, emojiName, delta); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM message_reactions WHERE message_id = $1::uuid AND emoji_key = $2 AND count = 0`,
		messageID, emojiKey); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RemoveAllReactions handles bulk reaction removal events.
func (s *Store) RemoveAllReactions(ctx context.Context, messageID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM message_reactions WHERE message_id = $1::uuid`, messageID)
	return err
}

// ListReactions returns the reaction aggregates for a message.
func (s *Store) ListReactions(ctx context.Context, messageID string) ([]Reaction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, message_id::text, emoji_key, emoji_name, count
		FROM message_reactions WHERE message_id = $1::uuid ORDER BY emoji_key`,
		messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Reaction
	for rows.Next() {
		var r Reaction
		if err := rows.Scan(&r.ID, &r.MessageID, &r.EmojiKey, &r.EmojiName, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Attachments and blobs

// AttachmentUpsert is the input for UpsertAttachment. The attachment
// record is created before its file is downloaded; the download pipeline
// later links a blob via MarkAttachmentStored.
type AttachmentUpsert struct {
	MessageID   string
	ExternalID  string
	Filename    string
	Description string
	ContentType string
	Size        int64
	SourceURL   string
	RawPayload  json.RawMessage
}

// UpsertAttachment inserts or updates attachment metadata and returns
// the attachment ID.
func (s *Store) UpsertAttachment(ctx context.Context, in AttachmentUpsert) (string, error) {
	if in.MessageID == "" {
		return "", fmt.Errorf("upsert attachment: message_id is required")
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO attachments (message_id, external_id, filename, description, content_type,
		                         size, source_url, raw_payload)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, COALESCE($8, '{}'::jsonb))
		ON CONFLICT (message_id, external_id) DO UPDATE SET
			filename     = EXCLUDED.filename,
			description  = EXCLUDED.description,
			content_type = EXCLUDED.content_type,
			size         = EXCLUDED.size,
			source_url   = EXCLUDED.source_url,
			raw_payload  = CASE WHEN EXCLUDED.raw_payload = '{}'::jsonb
			                    THEN attachments.raw_payload
			                    ELSE EXCLUDED.raw_payload END,
			updated_at   = now()
		RETURNING id::text`,
		in.MessageID, in.ExternalID, in.Filename, in.Description, in.ContentType,
		in.Size, in.SourceURL, in.RawPayload).Scan(&id)
	return id, err
}

// EnsureBlob records a stored physical file, deduplicating by SHA-256,
// and returns the blob ID.
func (s *Store) EnsureBlob(ctx context.Context, sha256hex string, size int64, contentType, objectKey string) (string, error) {
	if sha256hex == "" {
		return "", fmt.Errorf("ensure blob: sha256 is required")
	}
	var id string
	// DO UPDATE on the conflict target so RETURNING always yields the row.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO blobs (sha256, size, content_type, object_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sha256) DO UPDATE SET sha256 = EXCLUDED.sha256
		RETURNING id::text`,
		sha256hex, size, contentType, objectKey).Scan(&id)
	return id, err
}

// ErrBlobReferenced is returned when a blob cannot be deleted because an
// attachment still points at it.
var ErrBlobReferenced = errors.New("archive: blob is still referenced")

// OrphanBlob is a stored blob no attachment references any more.
type OrphanBlob struct {
	ID     string
	SHA256 string
}

// ListOrphanBlobs returns blobs nothing references, created before the
// given time. The cutoff is a grace period: a download creates its blob
// moments before linking it, and must not have it reclaimed in between.
func (s *Store) ListOrphanBlobs(ctx context.Context, createdBefore time.Time, limit int) ([]OrphanBlob, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.id::text, b.sha256
		FROM blobs b
		WHERE b.created_at < $1
		  AND NOT EXISTS (SELECT 1 FROM attachments a WHERE a.blob_id = b.id)
		ORDER BY b.created_at
		LIMIT $2`, createdBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrphanBlob
	for rows.Next() {
		var b OrphanBlob
		if err := rows.Scan(&b.ID, &b.SHA256); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBlob removes a blob row. It returns ErrBlobReferenced if an
// attachment has come to reference it since it was listed — the
// database's ON DELETE RESTRICT is what makes that safe rather than
// merely likely.
func (s *Store) DeleteBlob(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE id = $1::uuid`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrBlobReferenced
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// BlobExistsBySHA reports whether any blob row holds this digest. It
// lets reclamation notice that a download deduplicated onto content it
// was about to delete.
func (s *Store) BlobExistsBySHA(ctx context.Context, sha256hex string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blobs WHERE sha256 = $1)`, sha256hex).Scan(&exists)
	return exists, err
}

// MarkAttachmentStored links an attachment to its stored blob. Any
// download_error from an earlier failed attempt is cleared: a stale
// reason must not sit beside a file that is now successfully stored.
func (s *Store) MarkAttachmentStored(ctx context.Context, attachmentID, blobID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE attachments
		SET blob_id = $2::uuid, download_status = 'stored', download_error = NULL, updated_at = now()
		WHERE id = $1::uuid`,
		attachmentID, blobID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PendingAttachment is one attachment awaiting download.
type PendingAttachment struct {
	ID          string
	Filename    string
	ContentType string
	Size        int64
	SourceURL   string
}

const pendingAttachmentColumns = `a.id::text, a.filename, a.content_type, a.size, a.source_url`

func scanPendingAttachment(row pgx.Row) (PendingAttachment, error) {
	var a PendingAttachment
	err := row.Scan(&a.ID, &a.Filename, &a.ContentType, &a.Size, &a.SourceURL)
	return a, err
}

// ListPendingAttachments returns attachments whose file has not been
// stored yet, oldest first.
//
// Two exclusions, both about not fetching what the operator did not ask
// for. Attachments of tombstoned messages are skipped: deletion already
// removed that content, and fetching the file afterwards would put it
// back. Attachments in channels the operator has since disabled are
// skipped too, on the same predicate every other Discord-fetching path
// uses (threads inherit their parent's setting): downloads follow the
// current selection, so turning a channel off stops its downloads. What
// is already stored is kept — disabling has never meant deleting.
func (s *Store) ListPendingAttachments(ctx context.Context, limit int) ([]PendingAttachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+pendingAttachmentColumns+`
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN channels ch ON ch.id = m.channel_id
		LEFT JOIN channels par ON par.id = ch.parent_channel_id
		WHERE a.download_status = 'pending'
		  AND a.source_url <> ''
		  AND m.deleted_at IS NULL
		  AND (ch.archive_enabled OR COALESCE(par.archive_enabled, false))
		ORDER BY a.created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAttachment
	for rows.Next() {
		a, err := scanPendingAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAttachment loads one attachment by ID.
func (s *Store) GetAttachment(ctx context.Context, id string) (PendingAttachment, bool, error) {
	a, err := scanPendingAttachment(s.pool.QueryRow(ctx,
		`SELECT `+pendingAttachmentColumns+` FROM attachments a WHERE a.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return PendingAttachment{}, false, nil
	}
	if err != nil {
		return PendingAttachment{}, false, err
	}
	return a, true, nil
}

// GetStoredAttachment loads the canonical metadata and blob digest for an
// attachment whose bytes have been stored. Tombstoned messages cannot return
// content even if inconsistent rows somehow survive their cascading delete.
func (s *Store) GetStoredAttachment(ctx context.Context, id string) (StoredAttachment, bool, error) {
	var a StoredAttachment
	err := s.pool.QueryRow(ctx, `
		SELECT a.id::text, a.filename,
		       COALESCE(NULLIF(a.content_type, ''), b.content_type),
		       b.size, b.sha256
		FROM attachments a
		JOIN blobs b ON b.id = a.blob_id
		JOIN messages m ON m.id = a.message_id
		WHERE a.id = $1::uuid
		  AND a.download_status = 'stored'
		  AND m.deleted_at IS NULL`, id).
		Scan(&a.ID, &a.Filename, &a.ContentType, &a.Size, &a.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredAttachment{}, false, nil
	}
	if err != nil {
		return StoredAttachment{}, false, err
	}
	return a, true, nil
}

// SetAttachmentSourceURL records a refreshed download URL.
func (s *Store) SetAttachmentSourceURL(ctx context.Context, id, sourceURL string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE attachments SET source_url = $2, updated_at = now() WHERE id = $1::uuid`,
		id, sourceURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAttachmentFailed records a permanently failed attachment download
// and why. An already-stored attachment is never downgraded.
func (s *Store) MarkAttachmentFailed(ctx context.Context, attachmentID, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE attachments SET download_status = 'failed', download_error = $2, updated_at = now()
		WHERE id = $1::uuid AND download_status <> 'stored'`,
		attachmentID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Listings

// ListCommunities returns all communities, oldest first.
func (s *Store) ListCommunities(ctx context.Context) ([]Community, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+communityColumns+` FROM communities ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Community
	for rows.Next() {
		c, err := scanCommunity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListChannels returns a community's channels (threads included),
// ordered by position then name.
func (s *Store) ListChannels(ctx context.Context, communityID string) ([]Channel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+channelColumns+` FROM channels WHERE community_id = $1::uuid ORDER BY position, name`,
		communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChannel fetches a channel by archive ID.
func (s *Store) GetChannel(ctx context.Context, channelID string) (Channel, bool, error) {
	c, err := scanChannel(s.pool.QueryRow(ctx,
		`SELECT `+channelColumns+` FROM channels WHERE id = $1::uuid`, channelID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, false, nil
	}
	if err != nil {
		return Channel{}, false, err
	}
	return c, true, nil
}

// GetChannelBySourceExternalID fetches a channel by its identity on the
// source platform, across all communities of that source.
func (s *Store) GetChannelBySourceExternalID(ctx context.Context, source, externalID string) (Channel, bool, error) {
	c, err := scanChannel(s.pool.QueryRow(ctx, `
		SELECT `+prefixedChannelColumns("ch")+`
		FROM channels ch
		JOIN communities co ON co.id = ch.community_id
		WHERE co.source = $1 AND ch.external_id = $2`,
		source, externalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, false, nil
	}
	if err != nil {
		return Channel{}, false, err
	}
	return c, true, nil
}

const archiveChannelColumns = `ch.id::text, ch.community_id::text, co.name,
	ch.parent_channel_id::text, COALESCE(parent.name, ''), COALESCE(parent.kind, ''),
	ch.kind, ch.name, ch.topic, ch.position, ch.is_private, ch.is_archived,
	ch.archive_enabled, COALESCE(st.status, 'pending'),
	COALESCE(st.backfill_complete, false),
	(SELECT count(*) FROM messages m WHERE m.channel_id = ch.id AND m.deleted_at IS NULL),
	(SELECT max(m.source_created_at) FROM messages m WHERE m.channel_id = ch.id AND m.deleted_at IS NULL)`

func scanArchiveChannel(row pgx.Row) (ArchiveChannel, error) {
	var ch ArchiveChannel
	err := row.Scan(&ch.ID, &ch.CommunityID, &ch.CommunityName,
		&ch.ParentChannelID, &ch.ParentChannelName, &ch.ParentKind,
		&ch.Kind, &ch.Name, &ch.Topic, &ch.Position, &ch.IsPrivate, &ch.IsArchived,
		&ch.ArchiveEnabled, &ch.SyncStatus, &ch.BackfillComplete,
		&ch.MessageCount, &ch.LastMessageAt)
	return ch, err
}

// ListArchiveChannels returns channels that are currently selected, inherit a
// selected parent's setting, or retain live archived messages. The last case
// is important: disabling future ingestion never hides or deletes what was
// already archived.
func (s *Store) ListArchiveChannels(ctx context.Context) ([]ArchiveChannel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+archiveChannelColumns+`
		FROM channels ch
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN channels parent ON parent.id = ch.parent_channel_id
		LEFT JOIN sync_states st ON st.channel_id = ch.id
		WHERE ch.archive_enabled
		   OR COALESCE(parent.archive_enabled, false)
		   OR EXISTS (
		       SELECT 1 FROM messages m
		       WHERE m.channel_id = ch.id AND m.deleted_at IS NULL
		   )
		   OR (ch.kind <> 'category' AND EXISTS (
		       SELECT 1
		       FROM channels child
		       JOIN messages m ON m.channel_id = child.id AND m.deleted_at IS NULL
		       WHERE child.parent_channel_id = ch.id
		   ))
		ORDER BY co.name, COALESCE(parent.position, ch.position),
		         COALESCE(parent.name, ch.name), ch.position, ch.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchiveChannel
	for rows.Next() {
		ch, err := scanArchiveChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// GetArchiveChannel loads one browseable channel. Empty, unselected discovery
// rows are not part of the archive UI, while a disabled channel with retained
// messages remains browseable.
func (s *Store) GetArchiveChannel(ctx context.Context, channelID string) (ArchiveChannel, bool, error) {
	ch, err := scanArchiveChannel(s.pool.QueryRow(ctx, `
		SELECT `+archiveChannelColumns+`
		FROM channels ch
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN channels parent ON parent.id = ch.parent_channel_id
		LEFT JOIN sync_states st ON st.channel_id = ch.id
		WHERE ch.id = $1::uuid
		  AND (ch.archive_enabled
		       OR COALESCE(parent.archive_enabled, false)
		       OR EXISTS (
		           SELECT 1 FROM messages m
		           WHERE m.channel_id = ch.id AND m.deleted_at IS NULL
		       )
		       OR (ch.kind <> 'category' AND EXISTS (
		           SELECT 1
		           FROM channels child
		           JOIN messages m ON m.channel_id = child.id AND m.deleted_at IS NULL
		           WHERE child.parent_channel_id = ch.id
		       )))`, channelID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ArchiveChannel{}, false, nil
	}
	if err != nil {
		return ArchiveChannel{}, false, err
	}
	return ch, true, nil
}

const archiveMessageSelect = `
	SELECT m.id::text, m.channel_id::text, m.external_id, m.kind, m.content,
	       m.source_created_at, m.source_updated_at,
	       a.id::text, a.username, a.display_name, a.avatar_url, a.is_bot,
	       reply.id::text, reply.kind, reply.content, reply.source_created_at,
	       reply_actor.id::text, reply_actor.username, reply_actor.display_name,
	       reply_actor.avatar_url, reply_actor.is_bot,
	       -- Stickers stand in for the message body: a sticker-only message
	       -- has empty content and renders as nothing without them. The
	       -- archive keeps no sticker column by design, so the names come back
	       -- out of the payload the source sent. jsonb_typeof guards the
	       -- expansion because raw_payload is arbitrary source data, and one
	       -- malformed row must not fail the read for a whole channel.
	       COALESCE((
	           SELECT jsonb_agg(jsonb_build_object(
	               'id', sticker.value->>'id',
	               'name', sticker.value->>'name'
	           ) ORDER BY sticker.ordinality)
	           FROM jsonb_array_elements(CASE
	               WHEN jsonb_typeof(m.raw_payload->'sticker_items') = 'array'
	               THEN m.raw_payload->'sticker_items' ELSE '[]'::jsonb
	           END) WITH ORDINALITY AS sticker(value, ordinality)
	       ), '[]'::jsonb),
	       COALESCE((
	           SELECT jsonb_agg(jsonb_build_object(
	               'id', sticker.value->>'id',
	               'name', sticker.value->>'name'
	           ) ORDER BY sticker.ordinality)
	           FROM jsonb_array_elements(CASE
	               WHEN jsonb_typeof(reply.raw_payload->'sticker_items') = 'array'
	               THEN reply.raw_payload->'sticker_items' ELSE '[]'::jsonb
	           END) WITH ORDINALITY AS sticker(value, ordinality)
	       ), '[]'::jsonb),
	       COALESCE((
	           SELECT jsonb_agg(jsonb_build_object(
	               'id', att.id::text,
	               'filename', att.filename,
	               'description', att.description,
	               'content_type', att.content_type,
	               -- The stored file's own size once there is one. Discord's
	               -- CDN can serve a re-encoded copy whose size differs from
	               -- the metadata, and the number beside a download link has
	               -- to describe the file that link hands over.
	               'size', COALESCE(blob.size, att.size),
	               'download_status', att.download_status
	           ) ORDER BY att.created_at, att.id)
	           FROM attachments att
	           LEFT JOIN blobs blob ON blob.id = att.blob_id
	           WHERE att.message_id = m.id
	       ), '[]'::jsonb),
	       COALESCE((
	           SELECT jsonb_agg(jsonb_build_object(
	               'id', reaction.id::text,
	               'message_id', reaction.message_id::text,
	               'emoji_key', reaction.emoji_key,
	               'emoji_name', reaction.emoji_name,
	               'count', reaction.count
	           ) ORDER BY reaction.created_at, reaction.id)
	           FROM message_reactions reaction WHERE reaction.message_id = m.id
	       ), '[]'::jsonb),
	       (SELECT b.id::text FROM bookmarks b
	        WHERE b.message_id = m.id ORDER BY b.created_at, b.id LIMIT 1)
	FROM messages m
	LEFT JOIN actors a ON a.id = m.actor_id
	LEFT JOIN messages reply ON reply.id = m.reply_to_message_id AND reply.deleted_at IS NULL
	LEFT JOIN actors reply_actor ON reply_actor.id = reply.actor_id`

func scanArchiveMessage(row pgx.Row) (ArchiveMessage, error) {
	var m ArchiveMessage
	var actorID, actorUsername, actorDisplayName, actorAvatarURL *string
	var actorIsBot *bool
	var replyID, replyKind, replyContent *string
	var replyCreatedAt *time.Time
	var replyActorID, replyActorUsername, replyActorDisplayName, replyActorAvatarURL *string
	var replyActorIsBot *bool
	var attachmentsJSON, reactionsJSON, stickersJSON, replyStickersJSON json.RawMessage
	err := row.Scan(
		&m.ID, &m.ChannelID, &m.ExternalID, &m.Kind, &m.Content,
		&m.SourceCreatedAt, &m.SourceUpdatedAt,
		&actorID, &actorUsername, &actorDisplayName, &actorAvatarURL, &actorIsBot,
		&replyID, &replyKind, &replyContent, &replyCreatedAt,
		&replyActorID, &replyActorUsername, &replyActorDisplayName,
		&replyActorAvatarURL, &replyActorIsBot,
		&stickersJSON, &replyStickersJSON,
		&attachmentsJSON, &reactionsJSON, &m.BookmarkID,
	)
	if err != nil {
		return ArchiveMessage{}, err
	}
	if actorID != nil {
		m.Actor = &ArchiveActor{ID: *actorID}
		if actorUsername != nil {
			m.Actor.Username = *actorUsername
		}
		if actorDisplayName != nil {
			m.Actor.DisplayName = *actorDisplayName
		}
		if actorAvatarURL != nil {
			m.Actor.AvatarURL = *actorAvatarURL
		}
		if actorIsBot != nil {
			m.Actor.IsBot = *actorIsBot
		}
	}
	if replyID != nil && replyCreatedAt != nil {
		m.ReplyTo = &MessageReference{
			ID: *replyID, Content: replyContent, SourceCreatedAt: *replyCreatedAt,
		}
		if replyKind != nil {
			m.ReplyTo.Kind = *replyKind
		}
		if err := json.Unmarshal(replyStickersJSON, &m.ReplyTo.Stickers); err != nil {
			return ArchiveMessage{}, fmt.Errorf("decode reply preview stickers: %w", err)
		}
		if replyActorID != nil {
			m.ReplyTo.Actor = &ArchiveActor{ID: *replyActorID}
			if replyActorUsername != nil {
				m.ReplyTo.Actor.Username = *replyActorUsername
			}
			if replyActorDisplayName != nil {
				m.ReplyTo.Actor.DisplayName = *replyActorDisplayName
			}
			if replyActorAvatarURL != nil {
				m.ReplyTo.Actor.AvatarURL = *replyActorAvatarURL
			}
			if replyActorIsBot != nil {
				m.ReplyTo.Actor.IsBot = *replyActorIsBot
			}
		}
	}
	if err := json.Unmarshal(stickersJSON, &m.Stickers); err != nil {
		return ArchiveMessage{}, fmt.Errorf("decode archive message stickers: %w", err)
	}
	if err := json.Unmarshal(attachmentsJSON, &m.Attachments); err != nil {
		return ArchiveMessage{}, fmt.Errorf("decode archive message attachments: %w", err)
	}
	if err := json.Unmarshal(reactionsJSON, &m.Reactions); err != nil {
		return ArchiveMessage{}, fmt.Errorf("decode archive message reactions: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Bookmarks

const bookmarkColumns = `b.id::text, b.message_id::text, b.title, b.description,
	b.tags, b.collection, m.channel_id::text, ch.name, co.name, m.content,
	a.id::text, a.username, a.display_name, a.avatar_url, a.is_bot,
	m.source_created_at, b.created_at, b.updated_at`

type bookmarkQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanBookmark(row pgx.Row) (Bookmark, error) {
	var bookmark Bookmark
	var actorID, username, displayName, avatarURL *string
	var isBot *bool
	err := row.Scan(
		&bookmark.ID, &bookmark.MessageID, &bookmark.Title, &bookmark.Description,
		&bookmark.Tags, &bookmark.Collection, &bookmark.ChannelID, &bookmark.ChannelName,
		&bookmark.CommunityName, &bookmark.Content, &actorID, &username, &displayName,
		&avatarURL, &isBot, &bookmark.SourceCreatedAt, &bookmark.CreatedAt, &bookmark.UpdatedAt,
	)
	if err != nil {
		return Bookmark{}, err
	}
	if bookmark.Tags == nil {
		bookmark.Tags = []string{}
	}
	if actorID != nil {
		bookmark.Actor = &ArchiveActor{ID: *actorID}
		if username != nil {
			bookmark.Actor.Username = *username
		}
		if displayName != nil {
			bookmark.Actor.DisplayName = *displayName
		}
		if avatarURL != nil {
			bookmark.Actor.AvatarURL = *avatarURL
		}
		if isBot != nil {
			bookmark.Actor.IsBot = *isBot
		}
	}
	return bookmark, nil
}

func getBookmark(ctx context.Context, q bookmarkQuerier, where string, arg any) (Bookmark, bool, error) {
	bookmark, err := scanBookmark(q.QueryRow(ctx, `
		SELECT `+bookmarkColumns+`
		FROM bookmarks b
		JOIN messages m ON m.id = b.message_id AND m.deleted_at IS NULL
		JOIN channels ch ON ch.id = m.channel_id
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN actors a ON a.id = m.actor_id
		WHERE `+where+`
		ORDER BY b.created_at, b.id LIMIT 1`, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return Bookmark{}, false, nil
	}
	return bookmark, err == nil, err
}

// CreateBookmark saves a live archived message. Saving the same message again
// is idempotent and preserves metadata already curated in the UI.
func (s *Store) CreateBookmark(ctx context.Context, in BookmarkUpsert) (Bookmark, bool, error) {
	if in.MessageID == "" {
		return Bookmark{}, false, fmt.Errorf("create bookmark: message_id is required")
	}
	if in.Tags == nil {
		in.Tags = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Bookmark{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The original schema intentionally allowed multiple manual records. The
	// advisory lock makes application-created saves idempotent without an
	// unsafe migration that could discard an operator's existing duplicates.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, in.MessageID); err != nil {
		return Bookmark{}, false, err
	}
	if existing, found, err := getBookmark(ctx, tx, `b.message_id = $1::uuid`, in.MessageID); err != nil {
		return Bookmark{}, false, err
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return Bookmark{}, false, err
		}
		return existing, false, nil
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO bookmarks (message_id, title, description, tags, collection)
		SELECT id, $2, $3, $4, $5 FROM messages
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING id::text`, in.MessageID, in.Title, in.Description, in.Tags, in.Collection).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bookmark{}, false, ErrNotFound
	}
	if err != nil {
		return Bookmark{}, false, err
	}
	bookmark, _, err := getBookmark(ctx, tx, `b.id = $1::uuid`, id)
	if err != nil {
		return Bookmark{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Bookmark{}, false, err
	}
	return bookmark, true, nil
}

// CreateBookmarkBySourceIdentity is the source-agnostic entry point used by
// in-source save interactions. It never fetches or creates message content: a
// message must already have passed the ingest privacy gate.
func (s *Store) CreateBookmarkBySourceIdentity(ctx context.Context, source, channelExternalID, messageExternalID string) (Bookmark, bool, error) {
	var messageID string
	err := s.pool.QueryRow(ctx, `
		SELECT m.id::text
		FROM messages m
		JOIN channels ch ON ch.id = m.channel_id
		JOIN communities co ON co.id = ch.community_id
		WHERE co.source = $1 AND ch.external_id = $2 AND m.external_id = $3
		  AND m.deleted_at IS NULL`, source, channelExternalID, messageExternalID).Scan(&messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bookmark{}, false, ErrNotFound
	}
	if err != nil {
		return Bookmark{}, false, err
	}
	return s.CreateBookmark(ctx, BookmarkUpsert{MessageID: messageID})
}

// ListBookmarks returns curated messages newest first, optionally filtered by
// an exact collection or tag.
func (s *Store) ListBookmarks(ctx context.Context, filter BookmarkFilter) ([]Bookmark, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+bookmarkColumns+`
		FROM bookmarks b
		JOIN messages m ON m.id = b.message_id AND m.deleted_at IS NULL
		JOIN channels ch ON ch.id = m.channel_id
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN actors a ON a.id = m.actor_id
		WHERE ($1 = '' OR b.collection = $1)
		  AND ($2 = '' OR b.tags @> ARRAY[$2]::text[])
		ORDER BY b.created_at DESC, b.id DESC`, filter.Collection, filter.Tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bookmarks := []Bookmark{}
	for rows.Next() {
		bookmark, err := scanBookmark(rows)
		if err != nil {
			return nil, err
		}
		bookmarks = append(bookmarks, bookmark)
	}
	return bookmarks, rows.Err()
}

// UpdateBookmark replaces editable curation metadata.
func (s *Store) UpdateBookmark(ctx context.Context, id string, in BookmarkUpsert) (Bookmark, error) {
	if in.Tags == nil {
		in.Tags = []string{}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE bookmarks SET title = $2, description = $3, tags = $4,
			collection = $5, updated_at = now()
		WHERE id = $1::uuid`, id, in.Title, in.Description, in.Tags, in.Collection)
	if err != nil {
		return Bookmark{}, err
	}
	if tag.RowsAffected() == 0 {
		return Bookmark{}, ErrNotFound
	}
	bookmark, found, err := getBookmark(ctx, s.pool, `b.id = $1::uuid`, id)
	if err != nil {
		return Bookmark{}, err
	}
	if !found {
		return Bookmark{}, ErrNotFound
	}
	return bookmark, nil
}

// DeleteBookmark removes only curation metadata, never canonical message data.
func (s *Store) DeleteBookmark(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM bookmarks WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanArchiveMessages(rows pgx.Rows) ([]ArchiveMessage, error) {
	defer rows.Close()
	var messages []ArchiveMessage
	for rows.Next() {
		message, err := scanArchiveMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func reverseArchiveMessages(messages []ArchiveMessage) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

// ListMessages returns the newest page in chronological display order. A
// before cursor returns the next older page. Message IDs make pagination
// stable even when several messages share a timestamp.
func (s *Store) ListMessages(ctx context.Context, channelID, before string, limit int) (MessagePage, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	args := []any{channelID}
	where := `m.channel_id = $1::uuid AND m.deleted_at IS NULL`
	if before != "" {
		var cursorAt time.Time
		err := s.pool.QueryRow(ctx, `
			SELECT source_created_at FROM messages
			WHERE id = $1::uuid AND channel_id = $2::uuid AND deleted_at IS NULL`,
			before, channelID).Scan(&cursorAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return MessagePage{}, ErrNotFound
		}
		if err != nil {
			return MessagePage{}, err
		}
		args = append(args, cursorAt, before)
		where += ` AND (m.source_created_at, m.id) < ($2, $3::uuid)`
	}
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, archiveMessageSelect+`
		WHERE `+where+`
		ORDER BY m.source_created_at DESC, m.id DESC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return MessagePage{}, err
	}
	messages, err := scanArchiveMessages(rows)
	if err != nil {
		return MessagePage{}, err
	}
	hasOlder := len(messages) > limit
	if hasOlder {
		messages = messages[:limit]
	}
	reverseArchiveMessages(messages)
	if messages == nil {
		messages = []ArchiveMessage{}
	}
	return MessagePage{Messages: messages, HasOlder: hasOlder}, nil
}

// GetMessageContext returns one live message with beforeCount older and
// afterCount newer messages from the same channel. Deleted targets answer not
// found, and deleted surrounding messages never appear.
func (s *Store) GetMessageContext(ctx context.Context, messageID string, beforeCount, afterCount int) (MessageContext, bool, error) {
	if beforeCount < 0 {
		beforeCount = 0
	}
	if afterCount < 0 {
		afterCount = 0
	}
	if beforeCount > 50 {
		beforeCount = 50
	}
	if afterCount > 50 {
		afterCount = 50
	}
	var channelID string
	var targetAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT channel_id::text, source_created_at
		FROM messages WHERE id = $1::uuid AND deleted_at IS NULL`, messageID).
		Scan(&channelID, &targetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageContext{}, false, nil
	}
	if err != nil {
		return MessageContext{}, false, err
	}
	channel, ok, err := s.GetArchiveChannel(ctx, channelID)
	if err != nil || !ok {
		return MessageContext{}, false, err
	}

	beforeRows, err := s.pool.Query(ctx, archiveMessageSelect+`
		WHERE m.channel_id = $1::uuid AND m.deleted_at IS NULL
		  AND (m.source_created_at, m.id) < ($2, $3::uuid)
		ORDER BY m.source_created_at DESC, m.id DESC LIMIT $4`,
		channelID, targetAt, messageID, beforeCount)
	if err != nil {
		return MessageContext{}, false, err
	}
	before, err := scanArchiveMessages(beforeRows)
	if err != nil {
		return MessageContext{}, false, err
	}
	reverseArchiveMessages(before)

	target, err := scanArchiveMessage(s.pool.QueryRow(ctx, archiveMessageSelect+`
		WHERE m.id = $1::uuid AND m.deleted_at IS NULL`, messageID))
	if err != nil {
		return MessageContext{}, false, err
	}
	afterRows, err := s.pool.Query(ctx, archiveMessageSelect+`
		WHERE m.channel_id = $1::uuid AND m.deleted_at IS NULL
		  AND (m.source_created_at, m.id) > ($2, $3::uuid)
		ORDER BY m.source_created_at, m.id LIMIT $4`,
		channelID, targetAt, messageID, afterCount)
	if err != nil {
		return MessageContext{}, false, err
	}
	after, err := scanArchiveMessages(afterRows)
	if err != nil {
		return MessageContext{}, false, err
	}
	messages := make([]ArchiveMessage, 0, len(before)+1+len(after))
	messages = append(messages, before...)
	messages = append(messages, target)
	messages = append(messages, after...)
	return MessageContext{Channel: channel, TargetID: messageID, Messages: messages}, true, nil
}

// SearchMessages performs relevance-ranked full-text search over live
// canonical messages. The generated search_vector and every value returned by
// this method are derived from canonical rows, so the search layer can always
// be rebuilt without affecting the archive.
func (s *Store) SearchMessages(ctx context.Context, in SearchParams) (SearchPage, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return SearchPage{}, fmt.Errorf("search messages: query is required")
	}
	limit := in.Limit
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if in.Offset < 0 {
		in.Offset = 0
	}

	args := []any{query}
	where := []string{
		`m.deleted_at IS NULL`,
		`m.search_vector @@ search.query`,
	}
	addArg := func(value any) string {
		args = append(args, value)
		return "$" + fmt.Sprint(len(args))
	}
	if in.ChannelID != "" {
		where = append(where, `m.channel_id = `+addArg(in.ChannelID)+`::uuid`)
	}
	if author := strings.TrimSpace(in.Author); author != "" {
		placeholder := addArg(author)
		where = append(where, `(strpos(lower(a.username), lower(`+placeholder+`)) > 0
			OR strpos(lower(a.display_name), lower(`+placeholder+`)) > 0)`)
	}
	if in.After != nil {
		where = append(where, `m.source_created_at >= `+addArg(*in.After))
	}
	if in.Before != nil {
		where = append(where, `m.source_created_at < `+addArg(*in.Before))
	}
	if in.HasAttachment != nil {
		predicate := `EXISTS`
		if !*in.HasAttachment {
			predicate = `NOT EXISTS`
		}
		where = append(where, predicate+` (SELECT 1 FROM attachments filter_att WHERE filter_att.message_id = m.id)`)
	}

	limitArg := addArg(limit + 1)
	offsetArg := addArg(in.Offset)
	rows, err := s.pool.Query(ctx, `
		SELECT m.id::text, m.channel_id::text, ch.name, co.name,
		       a.id::text, a.username, a.display_name, a.avatar_url, a.is_bot,
		       m.source_created_at,
		       ts_headline('simple', COALESCE(m.content, ''), search.query,
		           'StartSel=<mark>, StopSel=</mark>, MaxWords=35, MinWords=15'),
		       EXISTS (SELECT 1 FROM attachments att WHERE att.message_id = m.id)
		FROM messages m
		JOIN channels ch ON ch.id = m.channel_id
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN actors a ON a.id = m.actor_id
		CROSS JOIN LATERAL websearch_to_tsquery('simple', $1) AS search(query)
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY ts_rank_cd(m.search_vector, search.query) DESC,
		         m.source_created_at DESC, m.id DESC
		LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return SearchPage{}, err
	}
	defer rows.Close()

	results := make([]SearchResult, 0, limit+1)
	for rows.Next() {
		var result SearchResult
		var actorID, username, displayName, avatarURL *string
		var isBot *bool
		if err := rows.Scan(
			&result.MessageID, &result.ChannelID, &result.ChannelName, &result.CommunityName,
			&actorID, &username, &displayName, &avatarURL, &isBot,
			&result.SourceCreatedAt, &result.Excerpt, &result.HasAttachment,
		); err != nil {
			return SearchPage{}, err
		}
		if actorID != nil {
			result.Actor = &ArchiveActor{ID: *actorID}
			if username != nil {
				result.Actor.Username = *username
			}
			if displayName != nil {
				result.Actor.DisplayName = *displayName
			}
			if avatarURL != nil {
				result.Actor.AvatarURL = *avatarURL
			}
			if isBot != nil {
				result.Actor.IsBot = *isBot
			}
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return SearchPage{}, err
	}
	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}
	return SearchPage{Results: results, HasMore: hasMore}, nil
}

// ---------------------------------------------------------------------------
// Sync state

const syncStateColumns = `id::text, channel_id::text, status, oldest_external_id, newest_external_id,
	backfill_complete, started_at, completed_at, last_synced_at, last_error`

func scanSyncState(row pgx.Row) (SyncState, error) {
	var st SyncState
	err := row.Scan(&st.ID, &st.ChannelID, &st.Status, &st.OldestExternalID, &st.NewestExternalID,
		&st.BackfillComplete, &st.StartedAt, &st.CompletedAt, &st.LastSyncedAt, &st.LastError)
	return st, err
}

// GetOrCreateSyncState returns the channel's sync state, creating a
// pending one on first use.
func (s *Store) GetOrCreateSyncState(ctx context.Context, channelID string) (SyncState, error) {
	return scanSyncState(s.pool.QueryRow(ctx, `
		INSERT INTO sync_states (channel_id)
		VALUES ($1::uuid)
		ON CONFLICT (channel_id) DO UPDATE SET channel_id = EXCLUDED.channel_id
		RETURNING `+syncStateColumns,
		channelID))
}

// SetSyncStatus updates the status; entering "importing" stamps
// started_at on first entry.
func (s *Store) SetSyncStatus(ctx context.Context, channelID, status string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_states SET
			status     = $2,
			started_at = CASE WHEN $2 = 'importing' THEN COALESCE(started_at, now()) ELSE started_at END,
			updated_at = now()
		WHERE channel_id = $1::uuid`,
		channelID, status)
	return err
}

// UpdateBackfillCheckpoint records the oldest message reached, making
// backfill resumable after any interruption.
func (s *Store) UpdateBackfillCheckpoint(ctx context.Context, channelID, oldestExternalID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_states SET oldest_external_id = $2, updated_at = now()
		WHERE channel_id = $1::uuid`,
		channelID, oldestExternalID)
	return err
}

// SetNewestSynced records the newest message observed for the channel.
func (s *Store) SetNewestSynced(ctx context.Context, channelID, newestExternalID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_states SET newest_external_id = $2, updated_at = now()
		WHERE channel_id = $1::uuid`,
		channelID, newestExternalID)
	return err
}

// MarkBackfillComplete transitions a channel to fully synced.
func (s *Store) MarkBackfillComplete(ctx context.Context, channelID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_states SET
			backfill_complete = true,
			status            = 'synced',
			completed_at      = COALESCE(completed_at, now()),
			last_synced_at    = now(),
			last_error        = '',
			updated_at        = now()
		WHERE channel_id = $1::uuid`,
		channelID)
	return err
}

// SetSyncError records a sync failure for operator visibility.
func (s *Store) SetSyncError(ctx context.Context, channelID, msg string) error {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_states SET status = 'error', last_error = $2, updated_at = now()
		WHERE channel_id = $1::uuid`,
		channelID, msg)
	return err
}

// TouchSynced records a successful sync pass.
func (s *Store) TouchSynced(ctx context.Context, channelID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_states SET status = 'synced', last_synced_at = now(), last_error = '', updated_at = now()
		WHERE channel_id = $1::uuid`,
		channelID)
	return err
}

// SyncOverview reports sync state for every enabled channel and the
// threads underneath them, for the dashboard and CLI.
func (s *Store) SyncOverview(ctx context.Context) ([]SyncOverviewRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ch.id::text, ch.name, co.name, ch.kind,
		       COALESCE(st.status, 'pending'),
		       COALESCE(st.backfill_complete, false),
		       st.last_synced_at,
		       COALESCE(st.last_error, ''),
		       (SELECT count(*) FROM messages m WHERE m.channel_id = ch.id AND m.deleted_at IS NULL)
		FROM channels ch
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN sync_states st ON st.channel_id = ch.id
		LEFT JOIN channels parent ON parent.id = ch.parent_channel_id
		WHERE ch.archive_enabled OR COALESCE(parent.archive_enabled, false)
		ORDER BY co.name, ch.position, ch.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncOverviewRow
	for rows.Next() {
		var r SyncOverviewRow
		if err := rows.Scan(&r.ChannelID, &r.ChannelName, &r.CommunityName, &r.Kind,
			&r.Status, &r.BackfillComplete, &r.LastSyncedAt, &r.LastError, &r.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Sync helpers

// ResolveReplyLinks fills reply_to_message_id for messages whose reply
// target arrived later (normal during newest→oldest backfill). Returns
// how many links were resolved.
func (s *Store) ResolveReplyLinks(ctx context.Context, channelID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE messages m SET reply_to_message_id = r.id, updated_at = now()
		FROM messages r
		WHERE m.channel_id = $1::uuid
		  AND m.reply_to_message_id IS NULL
		  AND m.reply_to_external_id IS NOT NULL
		  AND r.channel_id = m.channel_id
		  AND r.external_id = m.reply_to_external_id`,
		channelID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListArchivedExternalIDsSince returns the external IDs of live
// (non-deleted) archived messages created at or after the given time,
// used by reconciliation to detect deletions inside its window.
func (s *Store) ListArchivedExternalIDsSince(ctx context.Context, channelID string, since time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT external_id FROM messages
		WHERE channel_id = $1::uuid AND deleted_at IS NULL AND source_created_at >= $2
		ORDER BY source_created_at`,
		channelID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Status

// Counts returns archive totals for dashboards and CLI status output.
// Tombstoned messages are excluded: they are no longer archived content.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM communities),
			(SELECT count(*) FROM channels),
			(SELECT count(*) FROM messages WHERE deleted_at IS NULL),
			(SELECT count(*) FROM attachments),
			(SELECT count(*) FROM blobs)`,
	).Scan(&c.Communities, &c.Channels, &c.Messages, &c.Attachments, &c.Blobs)
	return c, err
}

// AttachmentStats summarizes the attachment pipeline's progress.
type AttachmentStats struct {
	Stored      int64
	Pending     int64
	Failed      int64
	StoredBytes int64
}

// AttachmentStats counts attachments by download state and totals the
// bytes held in blobs. Summing blobs rather than attachments counts a
// deduplicated file once; it is an estimate of disk use rather than a
// measurement, since it includes orphans not yet reclaimed and misses any
// file whose row was never written.
func (s *Store) AttachmentStats(ctx context.Context) (AttachmentStats, error) {
	var a AttachmentStats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM attachments WHERE download_status = 'stored'),
			(SELECT count(*) FROM attachments WHERE download_status = 'pending'),
			(SELECT count(*) FROM attachments WHERE download_status = 'failed'),
			(SELECT COALESCE(sum(size), 0) FROM blobs)`,
	).Scan(&a.Stored, &a.Pending, &a.Failed, &a.StoredBytes)
	return a, err
}
