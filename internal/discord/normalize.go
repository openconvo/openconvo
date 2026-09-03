package discord

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// rawMessage is the subset of a Discord message payload that
// normalization reads. Everything else is preserved verbatim in
// raw_payload, so nothing is lost by not modeling it here.
type rawMessage struct {
	ID              string          `json:"id"`
	ChannelID       string          `json:"channel_id"`
	Author          *rawUser        `json:"author"`
	Content         *string         `json:"content"`
	Timestamp       string          `json:"timestamp"`
	EditedTimestamp *string         `json:"edited_timestamp"`
	Type            *int            `json:"type"`
	MessageRef      *rawMessageRef  `json:"message_reference"`
	Attachments     []rawAttachment `json:"attachments"`
	Reactions       []rawReaction   `json:"reactions"`
}

type rawUser struct {
	ID         string  `json:"id"`
	Username   string  `json:"username"`
	GlobalName *string `json:"global_name"`
	Avatar     *string `json:"avatar"`
	Bot        bool    `json:"bot"`
}

type rawMessageRef struct {
	MessageID string `json:"message_id"`
}

type rawAttachment struct {
	ID          string  `json:"id"`
	Filename    string  `json:"filename"`
	Description *string `json:"description"`
	ContentType *string `json:"content_type"`
	Size        int64   `json:"size"`
	URL         string  `json:"url"`
}

type rawReaction struct {
	Emoji rawEmoji `json:"emoji"`
	Count int      `json:"count"`
}

type rawEmoji struct {
	ID   *string `json:"id"`
	Name string  `json:"name"`
}

// NormalizedMessage is a Discord message translated into the canonical
// archive vocabulary, ready for the archive store. It carries external
// IDs only; resolving them to archive rows is the ingester's job.
type NormalizedMessage struct {
	ExternalID        string
	ChannelExternalID string
	Author            *NormalizedActor
	Kind              string
	// Content is nil when the payload omitted content (partial update
	// events), which the archive treats as "leave existing content alone".
	Content           *string
	ReplyToExternalID *string
	CreatedAt         time.Time
	EditedAt          *time.Time
	Attachments       []NormalizedAttachment
	Reactions         []NormalizedReaction
	// Raw is the original payload, preserved verbatim.
	Raw json.RawMessage
}

// NormalizedActor is a Discord user translated into archive vocabulary.
type NormalizedActor struct {
	ExternalID  string
	Username    string
	DisplayName string
	AvatarURL   string
	IsBot       bool
}

// NormalizedAttachment is attachment metadata; the file itself is
// downloaded by the attachment pipeline.
type NormalizedAttachment struct {
	ExternalID  string
	Filename    string
	Description string
	ContentType string
	Size        int64
	SourceURL   string
}

// NormalizedReaction is an aggregate reaction count.
type NormalizedReaction struct {
	EmojiKey  string
	EmojiName string
	Count     int
}

// messageKinds maps Discord message type codes to archive kinds.
// Unmapped types become "type_<n>" so nothing is dropped; the mapping
// grows as message types prove relevant.
var messageKinds = map[int]string{
	0:  "default",
	6:  "pin",
	7:  "member_join",
	19: "reply",
	21: "thread_starter",
}

// NormalizeMessage converts a raw Discord message payload (Gateway
// event data or REST response) into the canonical archive shape.
func NormalizeMessage(payload json.RawMessage) (*NormalizedMessage, error) {
	var raw rawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("discord: parse message payload: %w", err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("discord: message payload has no id")
	}
	if raw.ChannelID == "" {
		return nil, fmt.Errorf("discord: message %s has no channel_id", raw.ID)
	}

	msg := &NormalizedMessage{
		ExternalID:        raw.ID,
		ChannelExternalID: raw.ChannelID,
		Content:           raw.Content,
		Raw:               payload,
	}

	if raw.Timestamp != "" {
		ts, err := parseTimestamp(raw.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("discord: message %s: %w", raw.ID, err)
		}
		msg.CreatedAt = ts
	} else {
		// Update events may omit the timestamp; derive creation time from
		// the snowflake so the message can still be placed on a timeline.
		msg.CreatedAt = SnowflakeTime(raw.ID)
	}

	if raw.EditedTimestamp != nil && *raw.EditedTimestamp != "" {
		ts, err := parseTimestamp(*raw.EditedTimestamp)
		if err != nil {
			return nil, fmt.Errorf("discord: message %s edited timestamp: %w", raw.ID, err)
		}
		msg.EditedAt = &ts
	}

	if raw.Type != nil {
		if kind, ok := messageKinds[*raw.Type]; ok {
			msg.Kind = kind
		} else {
			msg.Kind = fmt.Sprintf("type_%d", *raw.Type)
		}
	} else {
		msg.Kind = "default"
	}

	if raw.Author != nil && raw.Author.ID != "" {
		msg.Author = normalizeUser(raw.Author)
	}

	if raw.MessageRef != nil && raw.MessageRef.MessageID != "" {
		id := raw.MessageRef.MessageID
		msg.ReplyToExternalID = &id
	}

	for _, a := range raw.Attachments {
		att := NormalizedAttachment{
			ExternalID: a.ID,
			Filename:   a.Filename,
			Size:       a.Size,
			SourceURL:  a.URL,
		}
		if a.Description != nil {
			att.Description = *a.Description
		}
		if a.ContentType != nil {
			att.ContentType = *a.ContentType
		}
		msg.Attachments = append(msg.Attachments, att)
	}

	for _, r := range raw.Reactions {
		msg.Reactions = append(msg.Reactions, NormalizedReaction{
			EmojiKey:  EmojiKey(r.Emoji.ID, r.Emoji.Name),
			EmojiName: r.Emoji.Name,
			Count:     r.Count,
		})
	}

	return msg, nil
}

func normalizeUser(u *rawUser) *NormalizedActor {
	actor := &NormalizedActor{
		ExternalID:  u.ID,
		Username:    u.Username,
		DisplayName: u.Username,
		IsBot:       u.Bot,
	}
	if u.GlobalName != nil && *u.GlobalName != "" {
		actor.DisplayName = *u.GlobalName
	}
	if u.Avatar != nil && *u.Avatar != "" {
		actor.AvatarURL = AvatarURL(u.ID, *u.Avatar)
	}
	return actor
}

// EmojiKey builds the archive's emoji identity: the literal character
// for unicode emoji, "custom:<id>:<name>" for custom guild emoji.
func EmojiKey(customID *string, name string) string {
	if customID != nil && *customID != "" {
		return "custom:" + *customID + ":" + name
	}
	return name
}

// AvatarURL builds a CDN URL for a user avatar hash. Animated avatars
// (hash prefix "a_") are GIFs.
func AvatarURL(userID, hash string) string {
	ext := "png"
	if len(hash) > 2 && hash[:2] == "a_" {
		ext = "gif"
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.%s?size=256", userID, hash, ext)
}

// discordEpoch is the Discord snowflake epoch (2015-01-01T00:00:00Z)
// in Unix milliseconds.
const discordEpoch = 1420070400000

// SnowflakeTime extracts the creation time embedded in a Discord
// snowflake ID. Returns the zero time for unparseable input.
func SnowflakeTime(snowflake string) time.Time {
	var id uint64
	if _, err := fmt.Sscanf(snowflake, "%d", &id); err != nil {
		return time.Time{}
	}
	ms := int64(id>>22) + discordEpoch
	return time.UnixMilli(ms).UTC()
}

// parseTimestamp parses Discord's ISO 8601 timestamps.
func parseTimestamp(s string) (time.Time, error) {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timestamp %q: %w", s, err)
	}
	return ts.UTC(), nil
}

// NormalizedGuild is a Discord guild in archive vocabulary.
type NormalizedGuild struct {
	ExternalID  string
	Name        string
	Description string
	IconURL     string
	Raw         json.RawMessage
}

// NormalizeGuild converts a raw guild payload (REST or GUILD_CREATE).
func NormalizeGuild(payload json.RawMessage) (*NormalizedGuild, error) {
	var raw struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Description *string `json:"description"`
		Icon        *string `json:"icon"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("discord: parse guild payload: %w", err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("discord: guild payload has no id")
	}
	g := &NormalizedGuild{ExternalID: raw.ID, Name: raw.Name, Raw: payload}
	if raw.Description != nil {
		g.Description = *raw.Description
	}
	if raw.Icon != nil && *raw.Icon != "" {
		g.IconURL = fmt.Sprintf("https://cdn.discordapp.com/icons/%s/%s.png?size=256", raw.ID, *raw.Icon)
	}
	return g, nil
}

// channelKinds maps Discord channel type codes to archive kinds.
var channelKinds = map[int]string{
	0:  "text",
	2:  "voice",
	4:  "category",
	5:  "announcement",
	10: "thread", // announcement thread
	11: "thread", // public thread
	12: "private_thread",
	13: "stage",
	15: "forum",
	16: "media",
}

// threadTypes are the Discord channel types that are threads.
var threadTypes = map[int]bool{10: true, 11: true, 12: true}

// viewChannelPermission is Discord's VIEW_CHANNEL permission bit.
const viewChannelPermission = 1 << 10

// NormalizedChannel is a Discord channel or thread in archive vocabulary.
type NormalizedChannel struct {
	ExternalID string
	// GuildExternalID may be empty: channel objects nested in a guild
	// response omit it.
	GuildExternalID  string
	ParentExternalID string
	Kind             string
	Name             string
	Topic            string
	Position         int
	IsPrivate        bool
	IsThread         bool
	// IsArchived is the thread's archived state on Discord.
	IsArchived bool
	// ThreadArchiveTimestamp drives archived-thread pagination.
	ThreadArchiveTimestamp string
	CreatedAt              *time.Time
	Raw                    json.RawMessage
}

// Archivable reports whether the operator can select this channel for
// archiving: the kinds that contain messages or threads directly.
// Threads are archived through their parent, never selected directly.
func (c *NormalizedChannel) Archivable() bool {
	switch c.Kind {
	case "text", "announcement", "forum", "media":
		return true
	}
	return false
}

// NormalizeChannel converts a raw channel/thread payload.
func NormalizeChannel(payload json.RawMessage) (*NormalizedChannel, error) {
	var raw struct {
		ID                   string  `json:"id"`
		GuildID              string  `json:"guild_id"`
		Type                 int     `json:"type"`
		Name                 *string `json:"name"`
		Topic                *string `json:"topic"`
		Position             int     `json:"position"`
		ParentID             *string `json:"parent_id"`
		PermissionOverwrites []struct {
			ID   string `json:"id"`
			Type int    `json:"type"`
			Deny string `json:"deny"`
		} `json:"permission_overwrites"`
		ThreadMetadata *struct {
			Archived         bool   `json:"archived"`
			ArchiveTimestamp string `json:"archive_timestamp"`
		} `json:"thread_metadata"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("discord: parse channel payload: %w", err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("discord: channel payload has no id")
	}

	ch := &NormalizedChannel{
		ExternalID:      raw.ID,
		GuildExternalID: raw.GuildID,
		Position:        raw.Position,
		IsThread:        threadTypes[raw.Type],
		Raw:             payload,
	}
	if kind, ok := channelKinds[raw.Type]; ok {
		ch.Kind = kind
	} else {
		ch.Kind = fmt.Sprintf("type_%d", raw.Type)
	}
	if raw.Name != nil {
		ch.Name = *raw.Name
	}
	if raw.Topic != nil {
		ch.Topic = *raw.Topic
	}
	if raw.ParentID != nil {
		ch.ParentExternalID = *raw.ParentID
	}
	if ts := SnowflakeTime(raw.ID); !ts.IsZero() {
		ch.CreatedAt = &ts
	}
	if raw.ThreadMetadata != nil {
		ch.IsArchived = raw.ThreadMetadata.Archived
		ch.ThreadArchiveTimestamp = raw.ThreadMetadata.ArchiveTimestamp
	}
	// A channel is private when the @everyone role (whose ID equals the
	// guild ID) is denied VIEW_CHANNEL. Heuristic, but matches how
	// Discord's own UI marks private channels.
	for _, ow := range raw.PermissionOverwrites {
		if ow.Type != 0 || ow.ID != raw.GuildID {
			continue
		}
		if deny, err := strconv.ParseUint(ow.Deny, 10, 64); err == nil && deny&viewChannelPermission != 0 {
			ch.IsPrivate = true
		}
	}
	return ch, nil
}
