package preservation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DeletionRecord struct {
	ID          string     `json:"id"`
	Source      string     `json:"source"`
	ObjectType  string     `json:"object_type"`
	ExternalID  string     `json:"external_id"`
	DeletedAt   time.Time  `json:"deleted_at"`
	CreatedAt   time.Time  `json:"created_at"`
	ProcessedAt *time.Time `json:"processed_at"`
}

type ReplayReport struct {
	LedgerEntries      int64
	MessagesTombstoned int64
	ActorsScrubbed     int64
	AttachmentsDeleted int64
	ChannelsDeleted    int64
	CommunitiesDeleted int64
}

// ReplayDeletions applies an exported deletion ledger to a restored database
// in one transaction. Path may name a verified OpenConvo export directory or
// a standalone deletion_ledger.jsonl file.
func ReplayDeletions(ctx context.Context, pool *pgxpool.Pool, path string) (ReplayReport, error) {
	var report ReplayReport
	info, err := os.Stat(path)
	if err != nil {
		return report, fmt.Errorf("replay deletions: %w", err)
	}
	if info.IsDir() {
		if _, err := VerifyExport(ctx, path); err != nil {
			return report, fmt.Errorf("replay deletions: refusing unverified export: %w", err)
		}
		path = filepath.Join(path, "deletion_ledger.jsonl")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE replay_deletions (
		id uuid PRIMARY KEY,
		source text NOT NULL,
		object_type text NOT NULL,
		external_id text NOT NULL,
		deleted_at timestamptz NOT NULL,
		created_at timestamptz NOT NULL
	) ON COMMIT DROP`); err != nil {
		return report, err
	}

	batch := make([][]any, 0, 1000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"replay_deletions"},
			[]string{"id", "source", "object_type", "external_id", "deleted_at", "created_at"},
			pgx.CopyFromRows(batch))
		batch = batch[:0]
		return err
	}
	_, err = readRecords(path, func(line int, raw []byte) error {
		var record DeletionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.ID == "" || record.Source == "" || record.ExternalID == "" || record.DeletedAt.IsZero() {
			return fmt.Errorf("line %d: incomplete deletion record", line)
		}
		switch record.ObjectType {
		case "message", "actor", "attachment", "channel", "community":
		default:
			return fmt.Errorf("line %d: unsupported object_type %q", line, record.ObjectType)
		}
		if record.CreatedAt.IsZero() {
			record.CreatedAt = record.DeletedAt
		}
		batch = append(batch, []any{record.ID, record.Source, record.ObjectType, record.ExternalID, record.DeletedAt, record.CreatedAt})
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	if err := flush(); err != nil {
		return report, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM replay_deletions`).Scan(&report.LedgerEntries); err != nil {
		return report, err
	}

	// Preserve the ledger itself before applying cascades. An entry already
	// present in the restored database is marked processed without duplication.
	if _, err := tx.Exec(ctx, `
		INSERT INTO deletion_ledger (id, source, object_type, external_id, deleted_at, processed_at, created_at)
		SELECT id, source, object_type, external_id, deleted_at, now(), created_at
		FROM replay_deletions
		ON CONFLICT (id) DO UPDATE SET processed_at=now()`); err != nil {
		return report, err
	}

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE replay_messages (id uuid PRIMARY KEY, deleted_at timestamptz NOT NULL) ON COMMIT DROP`); err != nil {
		return report, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO replay_messages (id, deleted_at)
		SELECT m.id, min(d.deleted_at)
		FROM replay_deletions d
		JOIN communities co ON co.source=d.source
		JOIN channels ch ON ch.community_id=co.id
		JOIN messages m ON m.channel_id=ch.id AND m.external_id=d.external_id
		WHERE d.object_type='message'
		GROUP BY m.id
		ON CONFLICT (id) DO UPDATE SET deleted_at=LEAST(replay_messages.deleted_at, EXCLUDED.deleted_at)`); err != nil {
		return report, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO replay_messages (id, deleted_at)
		SELECT m.id, min(d.deleted_at)
		FROM replay_deletions d
		JOIN actors a ON a.source=d.source AND a.external_id=d.external_id
		JOIN messages m ON m.actor_id=a.id
		WHERE d.object_type='actor'
		GROUP BY m.id
		ON CONFLICT (id) DO UPDATE SET deleted_at=LEAST(replay_messages.deleted_at, EXCLUDED.deleted_at)`); err != nil {
		return report, err
	}
	for _, statement := range []string{
		`DELETE FROM attachments a USING replay_messages r WHERE a.message_id=r.id`,
		`DELETE FROM message_reactions mr USING replay_messages r WHERE mr.message_id=r.id`,
		`DELETE FROM bookmarks b USING replay_messages r WHERE b.message_id=r.id`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return report, err
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE messages m SET content=NULL,
			deleted_at=CASE WHEN m.deleted_at IS NULL THEN r.deleted_at ELSE LEAST(m.deleted_at, r.deleted_at) END,
			raw_payload='{}'::jsonb, updated_at=now()
		FROM replay_messages r WHERE m.id=r.id
		  AND (m.content IS NOT NULL OR m.deleted_at IS NULL OR m.raw_payload <> '{}'::jsonb)`)
	if err != nil {
		return report, err
	}
	report.MessagesTombstoned = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `
		UPDATE actors a SET username='deleted-user', display_name='', avatar_url='', raw_payload='{}'::jsonb, updated_at=now()
		FROM replay_deletions d
		WHERE d.object_type='actor' AND a.source=d.source AND a.external_id=d.external_id
		  AND (a.username <> 'deleted-user' OR a.display_name <> '' OR a.avatar_url <> '' OR a.raw_payload <> '{}'::jsonb)`)
	if err != nil {
		return report, err
	}
	report.ActorsScrubbed = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `
		DELETE FROM attachments a USING messages m, channels ch, communities co, replay_deletions d
		WHERE a.message_id=m.id AND m.channel_id=ch.id AND ch.community_id=co.id
		  AND d.object_type='attachment' AND co.source=d.source AND a.external_id=d.external_id`)
	if err != nil {
		return report, err
	}
	report.AttachmentsDeleted = tag.RowsAffected()

	// Channel and community ledger entries represent operator/source deletion
	// of the container itself. Cascades are intentional here: retaining their
	// children would directly defeat that deletion record.
	tag, err = tx.Exec(ctx, `
		DELETE FROM channels ch USING communities co, replay_deletions d
		WHERE ch.community_id=co.id AND d.object_type='channel'
		  AND co.source=d.source AND ch.external_id=d.external_id`)
	if err != nil {
		return report, err
	}
	report.ChannelsDeleted = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `
		DELETE FROM communities co USING replay_deletions d
		WHERE d.object_type='community' AND co.source=d.source AND co.external_id=d.external_id`)
	if err != nil {
		return report, err
	}
	report.CommunitiesDeleted = tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	return report, nil
}
