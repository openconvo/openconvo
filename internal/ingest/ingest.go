// Package ingest applies normalized source events to the canonical
// archive. It is the single write path shared by live Gateway events,
// historical backfill and reconciliation, which is what makes all three
// idempotent and consistent.
//
// Privacy gate: message content is written ONLY for channels the
// operator explicitly enabled (threads inherit their parent's setting).
// Community and channel *metadata* is always applied — discovery needs
// it, and it contains no conversation content.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/discord"
)

// Ingester applies normalized events to the archive store.
type Ingester struct {
	store           *archive.Store
	logger          *slog.Logger
	onMessageStored func(context.Context, string)

	mu sync.Mutex
	// channelCache maps a channel's external ID to what ingestion needs
	// on every message: its archive ID and whether archiving applies.
	channelCache map[string]cachedChannel
	// communityCache maps guild external IDs to community archive IDs.
	communityCache map[string]string
}

// WithMessageStored installs a best-effort notification for derived systems.
// It runs only after canonical content and dependents are safely stored; it
// must not become an alternate write path for message content.
func (in *Ingester) WithMessageStored(fn func(context.Context, string)) *Ingester {
	in.onMessageStored = fn
	return in
}

type cachedChannel struct {
	id string
	// enabled is the effective setting: the channel's own flag, or its
	// parent's for threads.
	enabled bool
}

// New creates an Ingester.
func New(store *archive.Store, logger *slog.Logger) *Ingester {
	return &Ingester{
		store:          store,
		logger:         logger.With("component", "ingest"),
		channelCache:   map[string]cachedChannel{},
		communityCache: map[string]string{},
	}
}

// InvalidateChannelCache drops a channel's cache entry; call after
// anything that may change enablement (selection toggles, channel or
// parent updates).
func (in *Ingester) InvalidateChannelCache(channelExternalID string) {
	in.mu.Lock()
	delete(in.channelCache, channelExternalID)
	in.mu.Unlock()
}

// InvalidateAllChannels drops every cached channel entry. Used after
// selection changes, which also affect the threads of the toggled
// channel.
func (in *Ingester) InvalidateAllChannels() {
	in.mu.Lock()
	in.channelCache = map[string]cachedChannel{}
	in.mu.Unlock()
}

// ApplyGuild upserts a community from a normalized guild.
func (in *Ingester) ApplyGuild(ctx context.Context, g *discord.NormalizedGuild) (archive.Community, error) {
	community, err := in.store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source:      archive.SourceDiscord,
		ExternalID:  g.ExternalID,
		Name:        g.Name,
		Description: g.Description,
		IconURL:     g.IconURL,
		RawPayload:  g.Raw,
	})
	if err != nil {
		return archive.Community{}, err
	}
	in.mu.Lock()
	in.communityCache[g.ExternalID] = community.ID
	in.mu.Unlock()
	return community, nil
}

// ApplyChannel upserts a channel or thread. guildExternalID may be ""
// when the payload carries its own guild_id (ch.GuildExternalID).
func (in *Ingester) ApplyChannel(ctx context.Context, guildExternalID string, ch *discord.NormalizedChannel) (archive.Channel, error) {
	guildExt := ch.GuildExternalID
	if guildExt == "" {
		guildExt = guildExternalID
	}
	if guildExt == "" {
		return archive.Channel{}, fmt.Errorf("ingest: channel %s has no guild", ch.ExternalID)
	}
	communityID, err := in.communityID(ctx, guildExt)
	if err != nil {
		return archive.Channel{}, err
	}

	upsert := archive.ChannelUpsert{
		CommunityID:     communityID,
		ExternalID:      ch.ExternalID,
		Kind:            ch.Kind,
		Name:            ch.Name,
		Topic:           ch.Topic,
		Position:        ch.Position,
		IsPrivate:       ch.IsPrivate,
		IsArchived:      ch.IsArchived,
		SourceCreatedAt: ch.CreatedAt,
		RawPayload:      ch.Raw,
	}
	if ch.ParentExternalID != "" {
		if parent, ok, err := in.store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, ch.ParentExternalID); err != nil {
			return archive.Channel{}, err
		} else if ok {
			upsert.ParentChannelID = &parent.ID
		}
	}

	stored, err := in.store.UpsertChannel(ctx, upsert)
	if err != nil {
		return archive.Channel{}, err
	}
	in.InvalidateChannelCache(ch.ExternalID)
	return stored, nil
}

// ChannelDeletedAtSource marks a channel as no longer live on the
// source platform. Archived content is deliberately preserved: the
// archive outliving the channel is the whole point.
func (in *Ingester) ChannelDeletedAtSource(ctx context.Context, channelExternalID string) error {
	ch, ok, err := in.store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, channelExternalID)
	if err != nil || !ok {
		return err
	}
	_, err = in.store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: ch.CommunityID,
		ExternalID:  ch.ExternalID,
		Kind:        ch.Kind,
		Name:        ch.Name,
		Topic:       ch.Topic,
		Position:    ch.Position,
		IsPrivate:   ch.IsPrivate,
		IsArchived:  true,
	})
	if err != nil {
		return err
	}
	in.InvalidateChannelCache(channelExternalID)
	return nil
}

// ApplyMessage writes a message (create or update) if its channel is
// enabled for archiving. Returns false when the channel is unknown or
// not enabled — the caller treats that as a normal drop, not an error.
func (in *Ingester) ApplyMessage(ctx context.Context, m *discord.NormalizedMessage) (bool, error) {
	ch, ok, err := in.resolveChannel(ctx, m.ChannelExternalID)
	if err != nil || !ok || !ch.enabled {
		return false, err
	}

	var actorID *string
	if m.Author != nil {
		actor, err := in.store.UpsertActor(ctx, archive.ActorUpsert{
			Source:      archive.SourceDiscord,
			ExternalID:  m.Author.ExternalID,
			Username:    m.Author.Username,
			DisplayName: m.Author.DisplayName,
			AvatarURL:   m.Author.AvatarURL,
			IsBot:       m.Author.IsBot,
		})
		if err != nil {
			return false, fmt.Errorf("ingest: upsert actor: %w", err)
		}
		actorID = &actor.ID
	}

	kind := m.Kind
	if kind == "" {
		kind = "default"
	}
	msg, err := in.store.UpsertMessage(ctx, archive.MessageUpsert{
		ChannelID:         ch.id,
		ActorID:           actorID,
		ExternalID:        m.ExternalID,
		Kind:              kind,
		Content:           m.Content,
		ReplyToExternalID: m.ReplyToExternalID,
		SourceCreatedAt:   m.CreatedAt,
		SourceUpdatedAt:   m.EditedAt,
		RawPayload:        m.Raw,
	})
	if err != nil {
		return false, fmt.Errorf("ingest: upsert message %s: %w", m.ExternalID, err)
	}
	if msg.Deleted() {
		// Stale event for a tombstoned message: nothing else to apply.
		return true, nil
	}

	for _, att := range m.Attachments {
		if _, err := in.store.UpsertAttachment(ctx, archive.AttachmentUpsert{
			MessageID:   msg.ID,
			ExternalID:  att.ExternalID,
			Filename:    att.Filename,
			Description: att.Description,
			ContentType: att.ContentType,
			Size:        att.Size,
			SourceURL:   att.SourceURL,
		}); err != nil {
			return false, fmt.Errorf("ingest: upsert attachment: %w", err)
		}
	}
	for _, r := range m.Reactions {
		if err := in.store.SetReaction(ctx, msg.ID, r.EmojiKey, r.EmojiName, r.Count, nil); err != nil {
			return false, fmt.Errorf("ingest: set reaction: %w", err)
		}
	}
	if in.onMessageStored != nil {
		in.onMessageStored(ctx, msg.ID)
	}
	return true, nil
}

// ApplyMessageDelete tombstones a message.
func (in *Ingester) ApplyMessageDelete(ctx context.Context, channelExternalID, messageExternalID string) (bool, error) {
	ch, ok, err := in.resolveChannel(ctx, channelExternalID)
	if err != nil || !ok {
		return false, err
	}
	if !ch.enabled {
		// Selection gates new content, not destructive source events. Continue
		// honoring deletion for a message already in the archive, while avoiding
		// a ledger record that reveals an identity from a never-selected channel.
		if _, found, err := in.store.GetMessageByExternalID(ctx, ch.id, messageExternalID); err != nil || !found {
			return false, err
		}
	}
	sourceCreatedAt := discord.SnowflakeTime(messageExternalID)
	if sourceCreatedAt.IsZero() {
		// Discord message IDs are snowflakes in production. Remaining defensive
		// here keeps a malformed deletion fail-closed: the content-free tombstone
		// uses observation time rather than allowing later content resurrection.
		sourceCreatedAt = time.Now().UTC()
	}
	return in.store.MarkMessageDeleted(ctx, archive.SourceDiscord, ch.id, messageExternalID, sourceCreatedAt)
}

// ApplyMessageDeleteBulk tombstones many messages (MESSAGE_DELETE_BULK)
// and reports how many were actually archived.
func (in *Ingester) ApplyMessageDeleteBulk(ctx context.Context, channelExternalID string, messageExternalIDs []string) (int, error) {
	deleted := 0
	for _, id := range messageExternalIDs {
		found, err := in.ApplyMessageDelete(ctx, channelExternalID, id)
		if err != nil {
			return deleted, err
		}
		if found {
			deleted++
		}
	}
	return deleted, nil
}

// ApplyReactionDelta applies a live reaction add (+1) or remove (-1).
func (in *Ingester) ApplyReactionDelta(ctx context.Context, channelExternalID, messageExternalID, emojiKey, emojiName string, delta int) (bool, error) {
	msgID, ok, err := in.resolveMessage(ctx, channelExternalID, messageExternalID)
	if err != nil || !ok {
		return false, err
	}
	return true, in.store.AdjustReaction(ctx, msgID, emojiKey, emojiName, delta)
}

// ApplyReactionClear removes all reactions from a message.
func (in *Ingester) ApplyReactionClear(ctx context.Context, channelExternalID, messageExternalID string) (bool, error) {
	msgID, ok, err := in.resolveMessage(ctx, channelExternalID, messageExternalID)
	if err != nil || !ok {
		return false, err
	}
	return true, in.store.RemoveAllReactions(ctx, msgID)
}

// ---------------------------------------------------------------------------

func (in *Ingester) communityID(ctx context.Context, guildExternalID string) (string, error) {
	in.mu.Lock()
	id, ok := in.communityCache[guildExternalID]
	in.mu.Unlock()
	if ok {
		return id, nil
	}
	// Not cached: upsert a shell community so channel metadata is never
	// lost; a later GUILD_CREATE or GetGuild fills in the name.
	community, err := in.store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source:     archive.SourceDiscord,
		ExternalID: guildExternalID,
	})
	if err != nil {
		return "", err
	}
	in.mu.Lock()
	in.communityCache[guildExternalID] = community.ID
	in.mu.Unlock()
	return community.ID, nil
}

func (in *Ingester) resolveChannel(ctx context.Context, externalID string) (cachedChannel, bool, error) {
	in.mu.Lock()
	cached, ok := in.channelCache[externalID]
	in.mu.Unlock()
	if ok {
		return cached, true, nil
	}

	ch, found, err := in.store.GetChannelBySourceExternalID(ctx, archive.SourceDiscord, externalID)
	if err != nil {
		return cachedChannel{}, false, err
	}
	if !found {
		in.logger.Debug("message for unknown channel dropped", "channel_external_id", externalID)
		return cachedChannel{}, false, nil
	}

	enabled := ch.ArchiveEnabled
	if !enabled && ch.ParentChannelID != nil {
		parent, ok, err := in.store.GetChannel(ctx, *ch.ParentChannelID)
		if err != nil {
			return cachedChannel{}, false, err
		}
		enabled = ok && parent.ArchiveEnabled
	}

	cached = cachedChannel{id: ch.ID, enabled: enabled}
	in.mu.Lock()
	in.channelCache[externalID] = cached
	in.mu.Unlock()
	return cached, true, nil
}

func (in *Ingester) resolveMessage(ctx context.Context, channelExternalID, messageExternalID string) (string, bool, error) {
	ch, ok, err := in.resolveChannel(ctx, channelExternalID)
	if err != nil || !ok || !ch.enabled {
		return "", false, err
	}
	msg, found, err := in.store.GetMessageByExternalID(ctx, ch.id, messageExternalID)
	if err != nil || !found {
		return "", false, err
	}
	return msg.ID, true, nil
}
