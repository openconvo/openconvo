package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/testutil"
)

func TestToolsReadTheArchiveStore(t *testing.T) {
	ctx := context.Background()
	store := archive.New(testutil.NewDB(t))
	community, err := store.UpsertCommunity(ctx, archive.CommunityUpsert{
		Source: archive.SourceDiscord, ExternalID: "mcp-guild", Name: "Example Community",
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.UpsertChannel(ctx, archive.ChannelUpsert{
		CommunityID: community.ID, ExternalID: "mcp-channel", Kind: "text", Name: "help",
	})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := store.UpsertActor(ctx, archive.ActorUpsert{
		Source: archive.SourceDiscord, ExternalID: "mcp-user", Username: "alice", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	post := func(externalID, kind, content string, replyTo *string, at time.Duration) archive.Message {
		t.Helper()
		message, err := store.UpsertMessage(ctx, archive.MessageUpsert{
			ChannelID: channel.ID, ActorID: &actor.ID, ExternalID: externalID, Kind: kind,
			Content: &content, ReplyToExternalID: replyTo, SourceCreatedAt: base.Add(at),
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	question := post("mcp-question", "default", "What time is the meetup?", nil, 0)
	questionExternalID := question.ExternalID
	answer := post("mcp-answer", "reply", "The meetup starts at seven.", &questionExternalID, time.Minute)
	deleted := post("mcp-deleted", "default", "A secret about the meetup.", nil, 2*time.Minute)
	if found, err := store.MarkMessageDeleted(ctx, archive.SourceDiscord, channel.ID, deleted.ExternalID, deleted.SourceCreatedAt); err != nil || !found {
		t.Fatalf("delete: found=%v err=%v", found, err)
	}

	client := connectClient(t, Deps{Archive: store})
	call := func(name string, args map[string]any, out any) string {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		text := resultText(t, result)
		if result.IsError {
			t.Fatalf("%s returned error: %s", name, text)
		}
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatal(err)
		}
		return text
	}

	var search SearchOutput
	text := call("search_messages", map[string]any{"query": "meetup", "channel_id": channel.ID}, &search)
	if len(search.Results) != 2 || strings.Contains(text, "secret") {
		t.Fatalf("search = %s", text)
	}
	for _, result := range search.Results {
		if result.MessageID == answer.ID &&
			(result.Content != "The meetup starts at seven." || result.Kind != "reply" ||
				result.ReplyToMessageID != question.ID || !strings.Contains(result.Excerpt, "<mark>meetup</mark>")) {
			t.Errorf("answer = %+v", result)
		}
	}

	var channels ChannelsOutput
	text = call("list_channels", map[string]any{}, &channels)
	if len(channels.Channels) != 1 || channels.Channels[0].ChannelID != channel.ID ||
		channels.Channels[0].Name != "help" || channels.Channels[0].MessageCount != 2 {
		t.Fatalf("channels = %s", text)
	}

	var conversation ContextOutput
	text = call("get_message_context", map[string]any{"message_id": strings.ToUpper(answer.ID)}, &conversation)
	if len(conversation.Messages) != 2 || conversation.Messages[0].MessageID != question.ID ||
		conversation.Messages[1].MessageID != answer.ID || conversation.MoreBefore || conversation.MoreAfter ||
		conversation.Channel.Name != "help" || strings.Contains(text, "secret") {
		t.Fatalf("context = %s", text)
	}
}
