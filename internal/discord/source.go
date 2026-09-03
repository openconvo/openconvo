package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/openconvo/openconvo/internal/archive"
)

// Source connects Discord to the archive ingester.
type Source struct {
	client *Client
	token  string
	mu     sync.RWMutex
	status RuntimeStatus
}

// RuntimeStatus is the non-secret, in-memory state of the Discord Gateway.
// It is safe to expose to an authenticated administrator.
type RuntimeStatus struct {
	Connected   bool
	BotUsername string
	LastError   string
}

// NewSource creates the Discord source from a bot token.
func NewSource(token string) *Source {
	return &Source{client: NewClient(token), token: token}
}

// Client exposes the underlying REST client.
func (s *Source) Client() *Client { return s.client }

// Status returns a snapshot of the current Gateway state.
func (s *Source) Status() RuntimeStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *Source) markConnected(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Connected = true
	if username != "" {
		s.status.BotUsername = username
	}
	s.status.LastError = ""
}

func (s *Source) markDisconnected(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Connected = false
	if err != nil {
		s.status.LastError = err.Error()
	}
}

// Ingester is the archive write path Run applies events through.
//
// It is declared here rather than imported: internal/ingest depends on
// this package for the normalized types, so depending on it from here
// would be an import cycle. *ingest.Ingester satisfies this interface.
type Ingester interface {
	ApplyGuild(ctx context.Context, g *NormalizedGuild) (archive.Community, error)
	ApplyChannel(ctx context.Context, guildExternalID string, ch *NormalizedChannel) (archive.Channel, error)
	ChannelDeletedAtSource(ctx context.Context, channelExternalID string) error
	ApplyMessage(ctx context.Context, m *NormalizedMessage) (bool, error)
	ApplyMessageDelete(ctx context.Context, channelExternalID, messageExternalID string) (bool, error)
	ApplyMessageDeleteBulk(ctx context.Context, channelExternalID string, messageExternalIDs []string) (int, error)
	ApplyReactionDelta(ctx context.Context, channelExternalID, messageExternalID, emojiKey, emojiName string, delta int) (bool, error)
	ApplyReactionClear(ctx context.Context, channelExternalID, messageExternalID string) (bool, error)
}

// SourceDeps are Run's collaborators.
type SourceDeps struct {
	Ingester Ingester
	// ApplicationID and Bookmarks enable the administrator-only Archive
	// message context-menu action. The action is disabled when either is absent.
	ApplicationID string
	Bookmarks     BookmarkSaver
	// OnResync is invoked when a gateway session was lost and replaced:
	// events may have been missed, so callers should reconcile.
	OnResync func()
	Logger   *slog.Logger
}

// Run connects to the Discord Gateway and applies events to the archive
// until ctx is cancelled. Returns *FatalGatewayError for unrecoverable
// conditions (invalid token, disallowed intents).
func (s *Source) Run(ctx context.Context, deps SourceDeps) error {
	defer s.markDisconnected(nil)
	base := deps.Logger
	if base == nil {
		base = slog.Default()
	}
	logger := base.With("component", "discord")
	if deps.Bookmarks != nil && deps.ApplicationID != "" {
		// Curation registration must never delay the live archive Gateway.
		go func() {
			if err := s.client.RegisterArchiveCommand(ctx, deps.ApplicationID); err != nil && ctx.Err() == nil {
				// Existing registrations keep working, and the next restart retries.
				logger.Error("register Archive message command failed", "error", err)
			}
		}()
	}

	// The gateway tags its own component; give it the untagged logger so
	// its lines carry one component, not two.
	gateway := NewGateway(s.client, s.token, GatewayOptions{
		Handler:      func(ev GatewayEvent) error { return s.handleEvent(ctx, deps, logger, ev) },
		OnReidentify: deps.OnResync,
		OnReady:      s.markConnected,
		OnDisconnect: s.markDisconnected,
		Logger:       base,
	})
	return gateway.Run(ctx)
}

func (s *Source) handleEvent(ctx context.Context, deps SourceDeps, logger *slog.Logger, ev GatewayEvent) error {
	err := s.applyEvent(ctx, deps, ev)
	if err != nil && ctx.Err() == nil {
		logger.Error("apply gateway event failed; reconnecting without acknowledging it",
			"event_type", ev.Type, "sequence", ev.Seq, "error", err)
	}
	return err
}

func (s *Source) applyEvent(ctx context.Context, deps SourceDeps, ev GatewayEvent) error {
	in := deps.Ingester
	switch ev.Type {
	case "GUILD_CREATE", "GUILD_UPDATE":
		guild, err := NormalizeGuild(ev.Data)
		if err != nil {
			return err
		}
		if _, err := in.ApplyGuild(ctx, guild); err != nil {
			return err
		}
		if ev.Type != "GUILD_CREATE" {
			return nil
		}
		var payload struct {
			Channels []json.RawMessage `json:"channels"`
			Threads  []json.RawMessage `json:"threads"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		for _, raw := range append(payload.Channels, payload.Threads...) {
			ch, err := NormalizeChannel(raw)
			if err != nil {
				return err
			}
			if _, err := in.ApplyChannel(ctx, guild.ExternalID, ch); err != nil {
				return err
			}
		}
		return nil

	case "CHANNEL_CREATE", "CHANNEL_UPDATE", "THREAD_CREATE", "THREAD_UPDATE":
		ch, err := NormalizeChannel(ev.Data)
		if err != nil {
			return err
		}
		_, err = in.ApplyChannel(ctx, "", ch)
		return err

	case "THREAD_LIST_SYNC":
		var payload struct {
			GuildID string            `json:"guild_id"`
			Threads []json.RawMessage `json:"threads"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		for _, raw := range payload.Threads {
			ch, err := NormalizeChannel(raw)
			if err != nil {
				return err
			}
			if _, err := in.ApplyChannel(ctx, payload.GuildID, ch); err != nil {
				return err
			}
		}
		return nil

	case "CHANNEL_DELETE", "THREAD_DELETE":
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		return in.ChannelDeletedAtSource(ctx, payload.ID)

	case "MESSAGE_CREATE", "MESSAGE_UPDATE":
		msg, err := NormalizeMessage(ev.Data)
		if err != nil {
			return err
		}
		_, err = in.ApplyMessage(ctx, msg)
		return err

	case "MESSAGE_DELETE":
		var payload struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		_, err := in.ApplyMessageDelete(ctx, payload.ChannelID, payload.ID)
		return err

	case "MESSAGE_DELETE_BULK":
		var payload struct {
			IDs       []string `json:"ids"`
			ChannelID string   `json:"channel_id"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		_, err := in.ApplyMessageDeleteBulk(ctx, payload.ChannelID, payload.IDs)
		return err

	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE":
		var payload struct {
			ChannelID string `json:"channel_id"`
			MessageID string `json:"message_id"`
			Emoji     struct {
				ID   *string `json:"id"`
				Name string  `json:"name"`
			} `json:"emoji"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		delta := 1
		if ev.Type == "MESSAGE_REACTION_REMOVE" {
			delta = -1
		}
		_, err := in.ApplyReactionDelta(ctx, payload.ChannelID, payload.MessageID,
			EmojiKey(payload.Emoji.ID, payload.Emoji.Name), payload.Emoji.Name, delta)
		return err

	case "MESSAGE_REACTION_REMOVE_ALL":
		var payload struct {
			ChannelID string `json:"channel_id"`
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return err
		}
		_, err := in.ApplyReactionClear(ctx, payload.ChannelID, payload.MessageID)
		return err

	case "INTERACTION_CREATE":
		return s.handleArchiveInteraction(ctx, deps.Bookmarks, ev.Data)
	}
	// Unhandled event types are normal: Discord dispatches far more than
	// an archive needs.
	return nil
}
