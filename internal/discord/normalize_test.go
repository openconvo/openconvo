package discord

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestNormalizeMessageFixture(t *testing.T) {
	payload := loadFixture(t, "message_create.json")
	msg, err := NormalizeMessage(payload)
	if err != nil {
		t.Fatalf("NormalizeMessage: %v", err)
	}

	if msg.ExternalID != "1140384234567890123" {
		t.Errorf("ExternalID = %s", msg.ExternalID)
	}
	if msg.ChannelExternalID != "998877665544332211" {
		t.Errorf("ChannelExternalID = %s", msg.ChannelExternalID)
	}
	if msg.Kind != "reply" {
		t.Errorf("Kind = %s, want reply (type 19)", msg.Kind)
	}
	if msg.Content == nil || *msg.Content == "" {
		t.Fatal("content missing")
	}
	want := time.Date(2026, 3, 14, 9, 26, 53, 589000000, time.UTC)
	if !msg.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %s, want %s", msg.CreatedAt, want)
	}
	if msg.EditedAt != nil {
		t.Errorf("EditedAt = %v, want nil", msg.EditedAt)
	}

	if msg.Author == nil {
		t.Fatal("author missing")
	}
	if msg.Author.ExternalID != "445566778899001122" || msg.Author.Username != "john" {
		t.Errorf("author = %+v", msg.Author)
	}
	if msg.Author.DisplayName != "John" {
		t.Errorf("DisplayName = %s, want global_name", msg.Author.DisplayName)
	}
	if msg.Author.AvatarURL == "" || msg.Author.IsBot {
		t.Errorf("author avatar/bot wrong: %+v", msg.Author)
	}

	if msg.ReplyToExternalID == nil || *msg.ReplyToExternalID != "1140380000000000042" {
		t.Errorf("ReplyToExternalID = %v", msg.ReplyToExternalID)
	}

	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Filename != "delamination.jpg" || att.Size != 431872 ||
		att.ContentType != "image/jpeg" || att.SourceURL == "" ||
		att.Description != "Photo of the delaminated deck" {
		t.Errorf("attachment = %+v", att)
	}

	if len(msg.Reactions) != 2 {
		t.Fatalf("reactions = %d, want 2", len(msg.Reactions))
	}
	if msg.Reactions[0].EmojiKey != "👍" || msg.Reactions[0].Count != 3 {
		t.Errorf("unicode reaction = %+v", msg.Reactions[0])
	}
	if msg.Reactions[1].EmojiKey != "custom:667788990011223344:kickflip" {
		t.Errorf("custom reaction key = %s", msg.Reactions[1].EmojiKey)
	}

	// The raw payload is preserved verbatim.
	if string(msg.Raw) != string(payload) {
		t.Error("raw payload not preserved verbatim")
	}
}

func TestNormalizePartialUpdateKeepsContentNil(t *testing.T) {
	// MESSAGE_UPDATE events can be partial (e.g. embed-only updates).
	// Content must stay nil so the archive doesn't erase existing text.
	payload := json.RawMessage(`{
		"id": "1140384234567890123",
		"channel_id": "998877665544332211",
		"embeds": [{"title": "resolved link preview"}]
	}`)
	msg, err := NormalizeMessage(payload)
	if err != nil {
		t.Fatalf("NormalizeMessage: %v", err)
	}
	if msg.Content != nil {
		t.Errorf("Content = %q, want nil for partial payload", *msg.Content)
	}
	// Creation time is recovered from the snowflake.
	if msg.CreatedAt.IsZero() {
		t.Error("CreatedAt not derived from snowflake")
	}
	if msg.CreatedAt.Year() < 2015 {
		t.Errorf("snowflake time implausible: %s", msg.CreatedAt)
	}
}

func TestNormalizeEmptyContentIsNotNil(t *testing.T) {
	// An explicit empty content (e.g. attachment-only message) is a real
	// value, distinct from an omitted field.
	payload := json.RawMessage(`{
		"id": "1140384234567890123",
		"channel_id": "998877665544332211",
		"content": "",
		"timestamp": "2026-03-14T09:26:53+00:00",
		"type": 0
	}`)
	msg, err := NormalizeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content == nil || *msg.Content != "" {
		t.Errorf("Content = %v, want explicit empty string", msg.Content)
	}
}

func TestNormalizeRejectsInvalidPayloads(t *testing.T) {
	cases := map[string]string{
		"no id":         `{"channel_id": "1"}`,
		"no channel":    `{"id": "1"}`,
		"bad timestamp": `{"id": "1", "channel_id": "2", "timestamp": "not-a-time"}`,
		"not json":      `[[[`,
	}
	for name, payload := range cases {
		if _, err := NormalizeMessage(json.RawMessage(payload)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNormalizeUnknownMessageType(t *testing.T) {
	payload := json.RawMessage(`{
		"id": "1", "channel_id": "2",
		"timestamp": "2026-03-14T09:26:53+00:00", "type": 46
	}`)
	msg, err := NormalizeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != "type_46" {
		t.Errorf("Kind = %s, want type_46", msg.Kind)
	}
}

func TestSnowflakeTime(t *testing.T) {
	// 1140384234567890123 >> 22 = 271888788835 ms after the Discord epoch.
	ts := SnowflakeTime("1140384234567890123")
	if ts.Year() != 2023 {
		t.Errorf("SnowflakeTime year = %d, want 2023", ts.Year())
	}
	if !SnowflakeTime("garbage").IsZero() {
		t.Error("invalid snowflake should give zero time")
	}
}

func TestAvatarURL(t *testing.T) {
	if got := AvatarURL("42", "abcdef"); got != "https://cdn.discordapp.com/avatars/42/abcdef.png?size=256" {
		t.Errorf("static avatar URL = %s", got)
	}
	if got := AvatarURL("42", "a_animated"); got != "https://cdn.discordapp.com/avatars/42/a_animated.gif?size=256" {
		t.Errorf("animated avatar URL = %s", got)
	}
}

func TestNormalizeGuildFixture(t *testing.T) {
	g, err := NormalizeGuild(loadFixture(t, "guild.json"))
	if err != nil {
		t.Fatal(err)
	}
	if g.ExternalID != "112233445566778899" || g.Name != "Fingerboard France" {
		t.Errorf("guild = %+v", g)
	}
	if g.Description != "La communauté fingerboard" {
		t.Errorf("description = %q", g.Description)
	}
	if g.IconURL != "https://cdn.discordapp.com/icons/112233445566778899/a1b2c3d4e5f6.png?size=256" {
		t.Errorf("icon = %q", g.IconURL)
	}
	if len(g.Raw) == 0 {
		t.Error("raw not preserved")
	}
	if _, err := NormalizeGuild(json.RawMessage(`{"name":"no id"}`)); err == nil {
		t.Error("guild without id accepted")
	}
}

func TestNormalizeChannelsFixture(t *testing.T) {
	var raws []json.RawMessage
	if err := json.Unmarshal(loadFixture(t, "channels.json"), &raws); err != nil {
		t.Fatal(err)
	}
	byID := map[string]*NormalizedChannel{}
	for _, raw := range raws {
		ch, err := NormalizeChannel(raw)
		if err != nil {
			t.Fatal(err)
		}
		byID[ch.ExternalID] = ch
	}

	deck := byID["998877665544332211"]
	if deck.Kind != "text" || deck.Name != "deck-making" || deck.Topic != "Veneer, molds, glue" {
		t.Errorf("deck = %+v", deck)
	}
	if deck.ParentExternalID != "990000000000000001" || deck.GuildExternalID != "112233445566778899" {
		t.Errorf("deck hierarchy = %+v", deck)
	}
	if deck.IsPrivate || deck.IsThread || !deck.Archivable() {
		t.Errorf("deck flags = %+v", deck)
	}
	if deck.CreatedAt == nil || deck.CreatedAt.IsZero() {
		t.Error("deck CreatedAt not derived from snowflake")
	}

	if cat := byID["990000000000000001"]; cat.Kind != "category" || cat.Archivable() {
		t.Errorf("category = %+v", cat)
	}
	if staff := byID["990000000000000002"]; !staff.IsPrivate {
		t.Errorf("staff channel not detected private: %+v", staff)
	}
	if forum := byID["990000000000000003"]; forum.Kind != "forum" || !forum.Archivable() {
		t.Errorf("forum = %+v", forum)
	}

	thread := byID["990000000000000004"]
	if thread.Kind != "thread" || !thread.IsThread || !thread.IsArchived {
		t.Errorf("thread = %+v", thread)
	}
	if thread.ThreadArchiveTimestamp != "2026-05-01T10:00:00+00:00" {
		t.Errorf("archive ts = %q", thread.ThreadArchiveTimestamp)
	}
	if thread.Archivable() {
		t.Error("threads are synced via their parent, not directly archivable")
	}
}

func TestNormalizeChannelRejectsInvalidPayloads(t *testing.T) {
	if _, err := NormalizeChannel(json.RawMessage(`{"type":0}`)); err == nil {
		t.Error("channel without id accepted")
	}
	if _, err := NormalizeChannel(json.RawMessage(`not json`)); err == nil {
		t.Error("unparseable channel accepted")
	}
	// An unknown channel type is preserved rather than dropped.
	ch, err := NormalizeChannel(json.RawMessage(`{"id":"1","type":99}`))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Kind != "type_99" || ch.Archivable() {
		t.Errorf("unknown type = %+v", ch)
	}
}
