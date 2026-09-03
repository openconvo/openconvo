package embeddings

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openconvo/openconvo/internal/archive"
)

const semanticQueryTimeout = 20 * time.Second

// SearchMessages embeds one administrator query and searches only the active,
// disposable generation. Canonical search remains in archive.SearchMessages;
// this method cannot become load-bearing for archive reads.
func (s *Service) SearchMessages(ctx context.Context, in archive.SearchParams) (archive.SearchPage, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return archive.SearchPage{}, fmt.Errorf("semantic search: query is required")
	}
	settings, _, err := s.effectiveSettings(ctx)
	if err != nil {
		return archive.SearchPage{}, err
	}
	if !settings.Enabled {
		return archive.SearchPage{}, ErrDisabled
	}
	if !s.configured() {
		return archive.SearchPage{}, ErrNotConfigured
	}
	generation, found, err := s.generation(ctx)
	if err != nil {
		return archive.SearchPage{}, err
	}
	if !found || generation.Status != "active" {
		return archive.SearchPage{}, ErrNotReady
	}

	// A logical restore intentionally omits vector values but retains generation
	// provenance. Do not present an empty restored index as a valid no-results
	// answer while its first healing sweep is still pending.
	var eligible, embedded bool
	if err := s.pool.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM messages WHERE deleted_at IS NULL AND content IS NOT NULL AND btrim(content) <> ''),
			EXISTS (SELECT 1 FROM derived.message_embeddings WHERE generation_id=$1::uuid)`,
		generation.ID).Scan(&eligible, &embedded); err != nil {
		return archive.SearchPage{}, fmt.Errorf("check semantic index readiness: %w", err)
	}
	if eligible && !embedded {
		return archive.SearchPage{}, ErrNotReady
	}

	embedCtx, cancel := context.WithTimeout(ctx, semanticQueryTimeout)
	vectors, err := s.embedder.Embed(embedCtx, []string{query})
	cancel()
	if err != nil {
		return archive.SearchPage{}, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	if len(vectors) != 1 {
		return archive.SearchPage{}, fmt.Errorf("%w: returned %d query vectors", ErrProvider, len(vectors))
	}
	literal, err := vectorLiteral(vectors[0])
	if err != nil {
		return archive.SearchPage{}, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	return s.searchByVector(ctx, generation.ID, literal, in)
}

func (s *Service) searchByVector(ctx context.Context, generationID, queryVector string, in archive.SearchParams) (archive.SearchPage, error) {
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

	args := []any{queryVector, generationID}
	where := []string{
		`e.generation_id = $2::uuid`,
		`m.deleted_at IS NULL`,
		`m.content IS NOT NULL`,
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

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return archive.SearchPage{}, fmt.Errorf("begin semantic search: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Metadata filters are applied after approximate HNSW candidates. Iterative
	// scanning asks pgvector 0.8+ to keep searching until the requested filtered
	// result count is satisfied while retaining strict distance order.
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = strict_order`); err != nil {
		return archive.SearchPage{}, fmt.Errorf("configure semantic search: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT m.id::text, m.channel_id::text, ch.name, co.name,
		       a.id::text, a.username, a.display_name, a.avatar_url, a.is_bot,
		       m.source_created_at,
		       CASE WHEN char_length(m.content) > 500
		            THEN left(m.content, 500) || '…' ELSE m.content END,
		       EXISTS (SELECT 1 FROM attachments att WHERE att.message_id = m.id)
		FROM derived.message_embeddings e
		JOIN messages m ON m.id = e.message_id
		JOIN channels ch ON ch.id = m.channel_id
		JOIN communities co ON co.id = ch.community_id
		LEFT JOIN actors a ON a.id = m.actor_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY e.embedding <=> $1::vector
		LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return archive.SearchPage{}, fmt.Errorf("semantic search: %w", err)
	}
	defer rows.Close()

	results := make([]archive.SearchResult, 0, limit+1)
	for rows.Next() {
		var result archive.SearchResult
		var actorID, username, displayName, avatarURL *string
		var isBot *bool
		if err := rows.Scan(
			&result.MessageID, &result.ChannelID, &result.ChannelName, &result.CommunityName,
			&actorID, &username, &displayName, &avatarURL, &isBot,
			&result.SourceCreatedAt, &result.Excerpt, &result.HasAttachment,
		); err != nil {
			return archive.SearchPage{}, fmt.Errorf("scan semantic result: %w", err)
		}
		if actorID != nil {
			result.Actor = &archive.ArchiveActor{ID: *actorID}
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
		return archive.SearchPage{}, fmt.Errorf("semantic search rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return archive.SearchPage{}, fmt.Errorf("finish semantic search: %w", err)
	}
	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}
	return archive.SearchPage{Results: results, HasMore: hasMore}, nil
}
