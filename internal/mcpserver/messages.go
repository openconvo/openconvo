package mcpserver

import (
	"time"
	"unicode/utf8"

	"github.com/openconvo/openconvo/internal/archive"
)

// searchContentLimit caps each search result's content, in characters.
const searchContentLimit = 2000

type Author struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

// Message is one archived message as the tools return it: no actor or
// attachment IDs, URLs, blob keys, source payloads or bookmark state.
type Message struct {
	MessageID        string       `json:"message_id"`
	Author           *Author      `json:"author,omitempty"`
	SourceCreatedAt  string       `json:"source_created_at"`
	EditedAt         string       `json:"edited_at,omitempty"`
	Kind             string       `json:"kind,omitempty"` // omitted for an ordinary message
	Content          string       `json:"content"`
	ContentTruncated bool         `json:"content_truncated,omitempty"`
	ReplyToMessageID string       `json:"reply_to_message_id,omitempty"`
	Stickers         []string     `json:"stickers,omitempty"`
	Attachments      []Attachment `json:"attachments,omitempty"`
	Reactions        []Reaction   `json:"reactions,omitempty"`
}

type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	Description string `json:"description,omitempty"`
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
}

type Channel struct {
	ChannelID     string `json:"channel_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Topic         string `json:"topic,omitempty"`
	ParentName    string `json:"parent_name,omitempty"`
	CommunityName string `json:"community_name"`
	MessageCount  int64  `json:"message_count"`
	LastMessageAt string `json:"last_message_at,omitempty"`
}

// messageView caps the content at limit characters when limit is positive.
func messageView(m archive.ArchiveMessage, limit int) Message {
	out := Message{MessageID: m.ID, SourceCreatedAt: formatTime(m.SourceCreatedAt)}
	if m.SourceUpdatedAt != nil {
		out.EditedAt = formatTime(*m.SourceUpdatedAt)
	}
	if m.Kind != "default" {
		out.Kind = m.Kind
	}
	if m.Content != nil {
		out.Content, out.ContentTruncated = truncate(*m.Content, limit)
	}
	out.Author = authorView(m.Actor)
	if m.ReplyTo != nil {
		out.ReplyToMessageID = m.ReplyTo.ID
	}
	for _, sticker := range m.Stickers {
		if sticker.Name != "" {
			out.Stickers = append(out.Stickers, sticker.Name)
		}
	}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, Attachment{
			Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, Description: a.Description,
		})
	}
	for _, r := range m.Reactions {
		emoji := r.EmojiName
		if emoji == "" {
			emoji = r.EmojiKey
		}
		out.Reactions = append(out.Reactions, Reaction{Emoji: emoji, Count: r.Count})
	}
	return out
}

func authorView(actor *archive.ArchiveActor) *Author {
	if actor == nil {
		return nil
	}
	return &Author{Username: actor.Username, DisplayName: actor.DisplayName, IsBot: actor.IsBot}
}

func channelView(ch archive.ArchiveChannel) Channel {
	out := Channel{
		ChannelID: ch.ID, Name: ch.Name, Kind: ch.Kind, Topic: ch.Topic,
		ParentName: ch.ParentChannelName, CommunityName: ch.CommunityName,
		MessageCount: ch.MessageCount,
	}
	if ch.LastMessageAt != nil {
		out.LastMessageAt = formatTime(*ch.LastMessageAt)
	}
	return out
}

func truncate(text string, limit int) (string, bool) {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	return string([]rune(text)[:limit]) + "…", true
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
