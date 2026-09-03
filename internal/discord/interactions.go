package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/openconvo/openconvo/internal/archive"
)

const (
	archiveCommandName                 = "Archive"
	interactionTypeApplicationCommand  = 2
	applicationCommandTypeMessage      = 3
	interactionResponseChannelMessage  = 4
	messageFlagEphemeral               = 1 << 6
	permissionManageGuild              = "32"
	applicationIntegrationGuildInstall = 0
	interactionContextGuild            = 0
)

// BookmarkSaver is the narrow canonical write surface needed by Discord's
// save interaction. The implementation only saves an already-ingested message;
// it cannot bypass the enabled-channel privacy gate or fetch message content.
type BookmarkSaver interface {
	CreateBookmarkBySourceIdentity(ctx context.Context, source, channelExternalID, messageExternalID string) (archive.Bookmark, bool, error)
}

// RegisterArchiveCommand idempotently creates or updates OpenConvo's global
// command by name. Manage Guild is required so the in-Discord curation action is an
// administrator tool, matching the private admin UI.
func (c *Client) RegisterArchiveCommand(ctx context.Context, applicationID string) error {
	applicationID = strings.TrimSpace(applicationID)
	if applicationID == "" {
		return fmt.Errorf("discord: application ID is required to register Archive command")
	}
	command := map[string]any{
		"name":                       archiveCommandName,
		"type":                       applicationCommandTypeMessage,
		"default_member_permissions": permissionManageGuild,
		"integration_types":          []int{applicationIntegrationGuildInstall},
		"contexts":                   []int{interactionContextGuild},
	}
	return c.post(ctx, "/applications/"+url.PathEscape(applicationID)+"/commands", command, nil)
}

type archiveInteraction struct {
	ID        string `json:"id"`
	Type      int    `json:"type"`
	Token     string `json:"token"`
	ChannelID string `json:"channel_id"`
	Data      struct {
		Name     string `json:"name"`
		Type     int    `json:"type"`
		TargetID string `json:"target_id"`
	} `json:"data"`
}

func (s *Source) handleArchiveInteraction(ctx context.Context, saver BookmarkSaver, raw json.RawMessage) error {
	var interaction archiveInteraction
	if err := json.Unmarshal(raw, &interaction); err != nil {
		return fmt.Errorf("decode interaction: %w", err)
	}
	if interaction.Type != interactionTypeApplicationCommand ||
		interaction.Data.Type != applicationCommandTypeMessage ||
		interaction.Data.Name != archiveCommandName {
		return nil
	}
	if saver == nil {
		return s.client.respondInteraction(ctx, interaction.ID, interaction.Token,
			"OpenConvo’s Archive action is not configured on this server.")
	}

	_, created, saveErr := saver.CreateBookmarkBySourceIdentity(ctx, archive.SourceDiscord,
		interaction.ChannelID, interaction.Data.TargetID)
	message := "Saved to OpenConvo bookmarks."
	switch {
	case errors.Is(saveErr, archive.ErrNotFound):
		message = "That message is not in an enabled OpenConvo archive channel."
	case saveErr != nil:
		message = "OpenConvo could not save that message. Check the server logs and try again."
	case !created:
		message = "That message is already in OpenConvo bookmarks."
	}
	responseErr := s.client.respondInteraction(ctx, interaction.ID, interaction.Token, message)
	if saveErr != nil && !errors.Is(saveErr, archive.ErrNotFound) {
		return errors.Join(saveErr, responseErr)
	}
	return responseErr
}

func (c *Client) respondInteraction(ctx context.Context, id, token, content string) error {
	if id == "" || token == "" {
		return fmt.Errorf("discord: interaction id and token are required")
	}
	body := map[string]any{
		"type": interactionResponseChannelMessage,
		"data": map[string]any{"content": content, "flags": messageFlagEphemeral},
	}
	return c.requestJSON(ctx, http.MethodPost,
		"/interactions/"+url.PathEscape(id)+"/"+url.PathEscape(token)+"/callback",
		body, nil, false)
}
