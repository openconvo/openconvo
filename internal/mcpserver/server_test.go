package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/embeddings"
)

const testChannelID = "0198c0de-0000-4000-8000-000000000123"

type fakeSearch struct {
	page   archive.SearchPage
	err    error
	params []archive.SearchParams
}

func (f *fakeSearch) SearchMessages(_ context.Context, params archive.SearchParams) (archive.SearchPage, error) {
	f.params = append(f.params, params)
	return f.page, f.err
}

type fakeArchive struct {
	fakeSearch
	channels       []archive.ArchiveChannel
	messages       map[string]archive.ArchiveMessage
	contexts       map[string]archive.MessageContext
	contextCalls   [][2]int // before and after counts asked of the store
	channelLookups int
}

func (f *fakeArchive) GetArchiveChannel(_ context.Context, id string) (archive.ArchiveChannel, bool, error) {
	f.channelLookups++
	for _, channel := range f.channels {
		if channel.ID == id {
			return channel, true, nil
		}
	}
	return archive.ArchiveChannel{}, false, nil
}

func (f *fakeArchive) ListArchiveChannels(context.Context) ([]archive.ArchiveChannel, error) {
	return f.channels, nil
}

func (f *fakeArchive) GetArchiveMessages(_ context.Context, ids []string) ([]archive.ArchiveMessage, error) {
	var out []archive.ArchiveMessage
	for _, id := range ids {
		if message, ok := f.messages[id]; ok {
			out = append(out, message)
		}
	}
	return out, nil
}

func (f *fakeArchive) GetMessageContext(_ context.Context, id string, before, after int) (archive.MessageContext, bool, error) {
	f.contextCalls = append(f.contextCalls, [2]int{before, after})
	conversation, ok := f.contexts[id]
	if !ok {
		return archive.MessageContext{}, false, nil
	}
	target := 0
	for i, message := range conversation.Messages {
		if message.ID == id {
			target = i
		}
	}
	start, end := max(0, target-before), min(len(conversation.Messages), target+after+1)
	conversation.Messages = conversation.Messages[start:end]
	return conversation, true, nil
}

// withMessages makes every search hit loadable, with the given content.
func (f *fakeArchive) withMessages(content string) *fakeArchive {
	f.messages = map[string]archive.ArchiveMessage{}
	for _, result := range f.page.Results {
		f.messages[result.MessageID] = archive.ArchiveMessage{
			ID: result.MessageID, ChannelID: result.ChannelID, Kind: "default",
			Content: &content, Actor: result.Actor, SourceCreatedAt: result.SourceCreatedAt,
		}
	}
	return f
}

func newFakeArchive(page archive.SearchPage) *fakeArchive {
	return &fakeArchive{
		fakeSearch: fakeSearch{page: page},
		channels: []archive.ArchiveChannel{{
			ID: testChannelID, Name: "woodworking", Kind: "text", CommunityName: "OpenConvo",
		}},
	}
}

func TestSearchMessagesToolRoutesFTSAndReturnsReducedResults(t *testing.T) {
	created := time.Date(2026, 8, 20, 11, 12, 13, 0, time.FixedZone("test", 8*60*60))
	keyword := newFakeArchive(archive.SearchPage{
		Results: []archive.SearchResult{{
			MessageID: "message-1", ChannelID: testChannelID,
			ChannelName: "woodworking", CommunityName: "OpenConvo",
			Actor: &archive.ArchiveActor{
				ID: "private-actor-id", Username: "john", DisplayName: "John",
				AvatarURL: "https://example.invalid/avatar", IsBot: false,
			},
			SourceCreatedAt: created, Excerpt: "Use <mark>hide glue</mark>.", HasAttachment: true,
		}},
		HasMore: true,
	})
	content := "Use hide glue. It gives you time to adjust."
	edited := created.Add(time.Hour)
	bookmarkID := "private-bookmark-id"
	keyword.messages = map[string]archive.ArchiveMessage{"message-1": {
		ID: "message-1", ChannelID: testChannelID, ExternalID: "private-external-id",
		Kind: "reply", Content: &content,
		Stickers: []archive.MessageSticker{{ID: "private-sticker-id", Name: "wave"}},
		Actor: &archive.ArchiveActor{
			ID: "private-actor-id", Username: "john", DisplayName: "John",
			AvatarURL: "https://example.invalid/avatar",
		},
		ReplyTo:         &archive.MessageReference{ID: "question-1", Kind: "default"},
		SourceCreatedAt: created, SourceUpdatedAt: &edited,
		Attachments: []archive.ArchiveAttachment{{
			ID: "private-attachment-id", Filename: "photo.jpg", Description: "A photo of the result",
			ContentType: "image/jpeg", Size: 2048, DownloadStatus: "stored",
		}},
		Reactions: []archive.Reaction{
			{ID: "private-reaction-id", EmojiKey: "👍", EmojiName: "👍", Count: 3},
			{ID: "private-custom-id", EmojiKey: "custom:42:thanks", EmojiName: "thanks", Count: 1},
		},
		BookmarkID: &bookmarkID,
	}}
	client := connectClient(t, Deps{Archive: keyword})

	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages",
		Arguments: map[string]any{
			"query": "  hide glue  ", "channel_id": testChannelID,
			"author": " John ", "after": "2026-08-01", "before": "2026-09-01T00:00:00Z",
			"has_attachment": true, "limit": 10, "offset": 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %s", resultText(t, result))
	}
	if len(keyword.params) != 1 {
		t.Fatalf("keyword searches = %d, want 1", len(keyword.params))
	}
	params := keyword.params[0]
	if params.Query != "hide glue" || params.ChannelID != testChannelID || params.Author != "John" ||
		params.After == nil || params.Before == nil || params.HasAttachment == nil || !*params.HasAttachment ||
		params.Limit != 10 || params.Offset != 20 {
		t.Fatalf("search params = %+v", params)
	}

	var output SearchOutput
	if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Results) != 1 || !output.HasMore || output.NextOffset != 21 {
		t.Fatalf("output = %+v", output)
	}
	got := output.Results[0]
	if got.Author == nil || got.Author.DisplayName != "John" || got.SourceCreatedAt != "2026-08-20T03:12:13Z" {
		t.Errorf("result = %+v", got)
	}
	if got.Excerpt != "Use <mark>hide glue</mark>." || got.Content != content || got.ContentTruncated {
		t.Errorf("text = excerpt %q, content %q (truncated %v)", got.Excerpt, got.Content, got.ContentTruncated)
	}
	if got.Kind != "reply" || got.ReplyToMessageID != "question-1" || got.EditedAt != "2026-08-20T04:12:13Z" ||
		len(got.Stickers) != 1 || got.Stickers[0] != "wave" {
		t.Errorf("message fields = %+v", got.Message)
	}
	if len(got.Attachments) != 1 || got.Attachments[0] != (Attachment{
		Filename: "photo.jpg", ContentType: "image/jpeg", Size: 2048, Description: "A photo of the result",
	}) {
		t.Errorf("attachments = %+v", got.Attachments)
	}
	if len(got.Reactions) != 2 || got.Reactions[0] != (Reaction{Emoji: "👍", Count: 3}) ||
		got.Reactions[1] != (Reaction{Emoji: "thanks", Count: 1}) {
		t.Errorf("reactions = %+v", got.Reactions)
	}
	text := resultText(t, result)
	for _, private := range []string{"private-", "avatar", "download", "bookmark"} {
		if strings.Contains(text, private) {
			t.Errorf("result exposed %q, which search clients do not need: %s", private, text)
		}
	}
	if strings.Contains(text, "distance") {
		t.Errorf("keyword result carries a semantic distance: %s", text)
	}
}

func TestSearchMessagesToolRoutesSemanticSearch(t *testing.T) {
	keyword := newFakeArchive(archive.SearchPage{})
	semantic := &fakeSearch{page: archive.SearchPage{Results: []archive.SearchResult{}}}
	client := connectClient(t, Deps{Archive: keyword, Semantic: semantic})

	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_messages",
		Arguments: map[string]any{"query": "advice for bonding wood", "mode": "semantic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %s", resultText(t, result))
	}
	if len(keyword.params) != 0 || len(semantic.params) != 1 {
		t.Fatalf("keyword calls = %d, semantic calls = %d", len(keyword.params), len(semantic.params))
	}
	if semantic.params[0].Limit != 25 || semantic.params[0].Offset != 0 {
		t.Errorf("defaults = %+v", semantic.params[0])
	}
}

func TestSearchMessagesToolValidatesFiltersBeforeSearching(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown mode", map[string]any{"query": "x", "mode": "hybrid"}, "mode"},
		{"channel UUID", map[string]any{"query": "x", "channel_id": "general"}, "UUID"},
		{"date", map[string]any{"query": "x", "after": "yesterday"}, "YYYY-MM-DD"},
		{"date order", map[string]any{"query": "x", "after": "2026-09-01", "before": "2026-08-01"}, "earlier"},
		{"limit", map[string]any{"query": "x", "limit": 101}, "100"},
		{"unknown argument", map[string]any{"query": "x", "sql": "select *"}, "sql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyword := newFakeArchive(archive.SearchPage{})
			client := connectClient(t, Deps{Archive: keyword})
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "search_messages", Arguments: tc.args,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(resultText(t, result), tc.want) {
				t.Fatalf("result = error:%v %q, want error containing %q", result.IsError, resultText(t, result), tc.want)
			}
			if len(keyword.params) != 0 {
				t.Fatalf("invalid input reached search: %+v", keyword.params)
			}
		})
	}
}

func TestSearchMessagesToolRejectsUnknownChannel(t *testing.T) {
	for _, mode := range []string{"fts", "semantic"} {
		t.Run(mode, func(t *testing.T) {
			keyword := newFakeArchive(archive.SearchPage{Results: []archive.SearchResult{}})
			semantic := &fakeSearch{page: archive.SearchPage{Results: []archive.SearchResult{}}}
			client := connectClient(t, Deps{Archive: keyword, Semantic: semantic})
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "search_messages",
				Arguments: map[string]any{
					"query": "anything", "mode": mode, "channel_id": "00000000-0000-0000-0000-000000000000",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(resultText(t, result), "unknown channel_id") ||
				!strings.Contains(resultText(t, result), "list_channels") {
				t.Fatalf("result = error:%v %q", result.IsError, resultText(t, result))
			}
		})
	}
}

func TestSearchMessagesToolLooksUpTheChannelOnlyForAnEmptyPage(t *testing.T) {
	keyword := newFakeArchive(archive.SearchPage{Results: []archive.SearchResult{{
		MessageID: "hit", ChannelID: testChannelID, SourceCreatedAt: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
	}}}).withMessages("hit")
	client := connectClient(t, Deps{Archive: keyword})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "hit", "channel_id": testChannelID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || keyword.channelLookups != 0 {
		t.Fatalf("error:%v %q after %d channel lookups", result.IsError, resultText(t, result), keyword.channelLookups)
	}

	keyword.page = archive.SearchPage{Results: []archive.SearchResult{}}
	result, err = client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "nothing", "channel_id": testChannelID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || keyword.channelLookups != 1 {
		t.Fatalf("error:%v %q after %d channel lookups", result.IsError, resultText(t, result), keyword.channelLookups)
	}
}

func TestSearchMessagesToolExplainsUnsearchableQuery(t *testing.T) {
	keyword := newFakeArchive(archive.SearchPage{})
	keyword.err = archive.ErrQueryNotSearchable
	client := connectClient(t, Deps{Archive: keyword})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "🔥"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(resultText(t, result), "emoji") {
		t.Fatalf("result = error:%v %q", result.IsError, resultText(t, result))
	}
}

func TestSearchMessagesToolReturnsSemanticDistance(t *testing.T) {
	distance := 0.123456
	semantic := &fakeSearch{page: archive.SearchPage{Results: []archive.SearchResult{{
		MessageID: "message-semantic", ChannelID: testChannelID,
		ChannelName: "woodworking", CommunityName: "OpenConvo",
		SourceCreatedAt: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
		Excerpt:         "Use hide glue.", Distance: &distance,
	}}}}
	archiveFake := newFakeArchive(semantic.page).withMessages("Use hide glue.")
	client := connectClient(t, Deps{Archive: archiveFake, Semantic: semantic})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "bonding wood", "mode": "semantic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output SearchOutput
	if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Results) != 1 || output.Results[0].Distance == nil || *output.Results[0].Distance != 0.1235 {
		t.Fatalf("output = %s", resultText(t, result))
	}
	if output.Results[0].Excerpt != "" || output.Results[0].Content != "Use hide glue." {
		t.Fatalf("semantic result text = %+v", output.Results[0])
	}
}

func TestSearchMessagesToolDropsHitsDeletedBeforeTheyAreRead(t *testing.T) {
	created := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	keyword := newFakeArchive(archive.SearchPage{Results: []archive.SearchResult{
		{MessageID: "kept", ChannelID: testChannelID, SourceCreatedAt: created, Excerpt: "kept"},
		{MessageID: "deleted", ChannelID: testChannelID, SourceCreatedAt: created, Excerpt: "deleted secret"},
	}, HasMore: true}).withMessages("kept in full")
	delete(keyword.messages, "deleted")
	client := connectClient(t, Deps{Archive: keyword})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "kept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output SearchOutput
	if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Results) != 1 || output.Results[0].MessageID != "kept" || !output.HasMore || output.NextOffset != 2 {
		t.Fatalf("output = %s", resultText(t, result))
	}
	if strings.Contains(resultText(t, result), "deleted secret") {
		t.Fatalf("a deleted message reached the client: %s", resultText(t, result))
	}
}

func TestSearchMessagesToolCapsLongContent(t *testing.T) {
	long := strings.Repeat("é", searchContentLimit+500)
	keyword := newFakeArchive(archive.SearchPage{Results: []archive.SearchResult{{
		MessageID: "long", ChannelID: testChannelID,
		SourceCreatedAt: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
	}}}).withMessages(long)
	client := connectClient(t, Deps{Archive: keyword})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "long"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output SearchOutput
	if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
		t.Fatal(err)
	}
	got := output.Results[0]
	if !got.ContentTruncated || got.Content != strings.Repeat("é", searchContentLimit)+"…" {
		t.Fatalf("content = %d runes, truncated %v", len([]rune(got.Content)), got.ContentTruncated)
	}
}

func conversationFixture(count, target int) (*fakeArchive, []string) {
	fake := newFakeArchive(archive.SearchPage{})
	ids := make([]string, count)
	messages := make([]archive.ArchiveMessage, count)
	created := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	for i := range ids {
		ids[i] = fmt.Sprintf("0198c0de-0000-4000-8000-%012d", i+1)
		content := fmt.Sprintf("message %d", i+1)
		messages[i] = archive.ArchiveMessage{
			ID: ids[i], ChannelID: testChannelID, Kind: "default", Content: &content,
			Actor:           &archive.ArchiveActor{ID: "private-actor-id", Username: "john", AvatarURL: "https://example.invalid/avatar"},
			SourceCreatedAt: created.Add(time.Duration(i) * time.Minute),
		}
	}
	fake.contexts = map[string]archive.MessageContext{ids[target]: {
		Channel: archive.ArchiveChannel{
			ID: testChannelID, Name: "first-thread", Kind: "thread", CommunityName: "OpenConvo",
			ParentChannelName: "help", MessageCount: int64(count),
		},
		TargetID: ids[target],
		Messages: messages,
	}}
	return fake, ids
}

func TestGetMessageContextReturnsTheConversationAround(t *testing.T) {
	cases := []struct {
		name                  string
		args                  map[string]any
		wantAsked             [2]int
		wantFirst, wantLast   int
		moreBefore, moreAfter bool
	}{
		{"defaults", map[string]any{}, [2]int{6, 6}, 1, 7, false, false},
		{"narrow window", map[string]any{"before": 2, "after": 1}, [2]int{3, 2}, 2, 5, true, true},
		{"target only", map[string]any{"before": 0, "after": 0}, [2]int{1, 1}, 4, 4, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, ids := conversationFixture(7, 3)
			client := connectClient(t, Deps{Archive: fake})
			args := map[string]any{"message_id": ids[3]}
			for key, value := range tc.args {
				args[key] = value
			}
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "get_message_context", Arguments: args,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("tool returned error: %s", resultText(t, result))
			}
			if len(fake.contextCalls) != 1 || fake.contextCalls[0] != tc.wantAsked {
				t.Fatalf("store asked for %v, want %v", fake.contextCalls, tc.wantAsked)
			}
			var output ContextOutput
			if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
				t.Fatal(err)
			}
			first, last := output.Messages[0], output.Messages[len(output.Messages)-1]
			if first.Content != fmt.Sprintf("message %d", tc.wantFirst) || last.Content != fmt.Sprintf("message %d", tc.wantLast) ||
				len(output.Messages) != tc.wantLast-tc.wantFirst+1 {
				t.Fatalf("messages %q … %q (%d)", first.Content, last.Content, len(output.Messages))
			}
			if output.TargetMessageID != ids[3] || output.MoreBefore != tc.moreBefore || output.MoreAfter != tc.moreAfter {
				t.Fatalf("target %s, more before %v, more after %v", output.TargetMessageID, output.MoreBefore, output.MoreAfter)
			}
			if output.Channel.ChannelID != testChannelID || output.Channel.Name != "first-thread" ||
				output.Channel.Kind != "thread" || output.Channel.ParentName != "help" {
				t.Fatalf("channel = %+v", output.Channel)
			}
			for _, private := range []string{"private-", "avatar"} {
				if strings.Contains(resultText(t, result), private) {
					t.Fatalf("context exposed %q: %s", private, resultText(t, result))
				}
			}
		})
	}
}

func TestGetMessageContextReturnsFullContent(t *testing.T) {
	fake, ids := conversationFixture(1, 0)
	long := strings.Repeat("é", searchContentLimit+500)
	fake.contexts[ids[0]].Messages[0].Content = &long
	client := connectClient(t, Deps{Archive: fake})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_message_context", Arguments: map[string]any{"message_id": ids[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output ContextOutput
	if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Messages) != 1 || output.Messages[0].Content != long || output.Messages[0].ContentTruncated {
		t.Fatalf("content = %d runes", len([]rune(output.Messages[0].Content)))
	}
}

func TestGetMessageContextAcceptsUpperCaseIDs(t *testing.T) {
	fake, ids := conversationFixture(3, 1)
	client := connectClient(t, Deps{Archive: fake})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_message_context", Arguments: map[string]any{"message_id": strings.ToUpper(ids[1])},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output ContextOutput
	if result.IsError || json.Unmarshal([]byte(resultText(t, result)), &output) != nil ||
		output.TargetMessageID != ids[1] || len(output.Messages) != 3 {
		t.Fatalf("error:%v %s", result.IsError, resultText(t, result))
	}
}

func TestGetMessageContextErrors(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"not a UUID", map[string]any{"message_id": "general"}, "UUID"},
		{"unknown or deleted", map[string]any{"message_id": "00000000-0000-4000-8000-000000000000"}, "message not found"},
		{"window too wide", map[string]any{"message_id": "00000000-0000-4000-8000-000000000000", "before": 26}, "25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := conversationFixture(3, 1)
			client := connectClient(t, Deps{Archive: fake})
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "get_message_context", Arguments: tc.args,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(resultText(t, result), tc.want) {
				t.Fatalf("result = error:%v %q, want error containing %q", result.IsError, resultText(t, result), tc.want)
			}
		})
	}
}

func TestListChannelsToolListsArchivedChannels(t *testing.T) {
	fake := newFakeArchive(archive.SearchPage{})
	last := time.Date(2026, 9, 24, 18, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	fake.channels = []archive.ArchiveChannel{
		{ID: "category-id", Kind: "category", Name: "Community", CommunityName: "OpenConvo", ArchiveEnabled: true},
		{
			ID: testChannelID, Kind: "text", Name: "help", Topic: "Questions and answers",
			ParentChannelID: strPtr("category-id"), ParentChannelName: "Community", ParentKind: "category",
			CommunityName: "OpenConvo", MessageCount: 57, LastMessageAt: &last,
			IsPrivate: true, ArchiveEnabled: true, SyncStatus: "synced", BackfillComplete: true,
		},
		{
			ID: "thread-id", Kind: "thread", Name: "Which option is better?", ParentChannelName: "help", ParentKind: "text",
			CommunityName: "OpenConvo", MessageCount: 4, SyncStatus: "importing",
		},
	}
	client := connectClient(t, Deps{Archive: fake})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_channels"})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %s", resultText(t, result))
	}
	var output ChannelsOutput
	if err := json.Unmarshal([]byte(resultText(t, result)), &output); err != nil {
		t.Fatal(err)
	}
	want := []Channel{
		{
			ChannelID: testChannelID, Name: "help", Kind: "text", Topic: "Questions and answers",
			ParentName: "Community", CommunityName: "OpenConvo", MessageCount: 57,
			LastMessageAt: "2026-09-24T16:30:00Z",
		},
		{ChannelID: "thread-id", Name: "Which option is better?", Kind: "thread", ParentName: "help", CommunityName: "OpenConvo", MessageCount: 4},
	}
	if len(output.Channels) != len(want) || output.Channels[0] != want[0] || output.Channels[1] != want[1] {
		t.Fatalf("channels = %+v, want %+v", output.Channels, want)
	}
	for _, operational := range []string{"sync", "private", "backfill", "category-id"} {
		if strings.Contains(resultText(t, result), operational) {
			t.Errorf("list exposed %q: %s", operational, resultText(t, result))
		}
	}
}

func TestSearchMessagesToolExplainsSemanticState(t *testing.T) {
	semantic := &fakeSearch{err: embeddings.ErrDisabled}
	client := connectClient(t, Deps{Archive: newFakeArchive(archive.SearchPage{}), Semantic: semantic})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "x", "mode": "semantic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(resultText(t, result), "disabled") {
		t.Fatalf("result = error:%v %q", result.IsError, resultText(t, result))
	}
}

func TestServerAdvertisesOnlyReadOnlyTools(t *testing.T) {
	client := connectClient(t, Deps{Archive: newFakeArchive(archive.SearchPage{})})
	listed, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s annotations = %+v", tool.Name, tool.Annotations)
		}
	}
	if len(tools) != 3 || tools["search_messages"] == nil || tools["get_message_context"] == nil || tools["list_channels"] == nil {
		t.Fatalf("tools = %+v", listed.Tools)
	}
	tool := tools["search_messages"]
	if !strings.Contains(tool.Description, "non-deleted") {
		t.Errorf("description does not state deletion behavior: %q", tool.Description)
	}
	for _, behavior := range []string{"ignoring letter case and accents", "emoji", "…", "distance", "midnight UTC", "get_message_context", "list_channels"} {
		if !strings.Contains(tool.Description, behavior) {
			t.Errorf("description does not mention %q: %q", behavior, tool.Description)
		}
	}
}

func TestUnexpectedSearchErrorsAreNotExposed(t *testing.T) {
	keyword := newFakeArchive(archive.SearchPage{})
	keyword.err = errors.New("postgres password secret")
	client := connectClient(t, Deps{Archive: keyword})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || resultText(t, result) != "search failed" {
		t.Fatalf("result = error:%v %q", result.IsError, resultText(t, result))
	}
}

func TestHTTPHandlerRequiresDedicatedBearerToken(t *testing.T) {
	server := New(Deps{Archive: newFakeArchive(archive.SearchPage{})}, "test")
	if _, err := NewHTTPHandler(nil, strings.Repeat("a", 32), nil); err == nil {
		t.Fatal("NewHTTPHandler accepted a nil server")
	}
	if _, err := NewHTTPHandler(server, "too-short", nil); err == nil {
		t.Fatal("NewHTTPHandler accepted a short token")
	}

	token := strings.Repeat("b", 64)
	handler, err := NewHTTPHandler(server, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(authorization string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "https://archive.example.com/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, authorization := range []string{"", "Basic abc", "Bearer wrong", "Bearer " + token + " extra"} {
		rec := request(authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("authorization %q = %d, want 401", authorization, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" || rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Errorf("authorization %q headers = %+v", authorization, rec.Header())
		}
	}
	if rec := request("bEaReR " + token); rec.Code != http.StatusOK {
		t.Fatalf("valid bearer status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandlerRejectsCrossOriginRequests(t *testing.T) {
	token := strings.Repeat("c", 64)
	handler, err := NewHTTPHandler(New(Deps{Archive: newFakeArchive(archive.SearchPage{})}, "test"), token, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://archive.example.com/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestSearchMessagesOverAuthenticatedHTTP(t *testing.T) {
	token := strings.Repeat("d", 64)
	keyword := newFakeArchive(archive.SearchPage{Results: []archive.SearchResult{{
		MessageID: "message-http", ChannelID: testChannelID,
		ChannelName: "general", CommunityName: "OpenConvo",
		SourceCreatedAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		Excerpt:         "remote result",
	}}}).withMessages("remote result in full")
	handler, err := NewHTTPHandler(New(Deps{Archive: keyword}, "test"), token, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	httpClient := &http.Client{Transport: bearerTransport{token: token}}
	transport := &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_messages", Arguments: map[string]any{"query": "remote"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(resultText(t, result), "remote result") {
		t.Fatalf("HTTP tool result = error:%v %s", result.IsError, resultText(t, result))
	}
	if len(keyword.params) != 1 || keyword.params[0].Query != "remote" {
		t.Fatalf("HTTP search params = %+v", keyword.params)
	}
}

type bearerTransport struct {
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func connectClient(t *testing.T, deps Deps) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(deps, "test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "openconvo-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clientSession.Close()
		serverSession.Close()
	})
	return clientSession
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content = %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T", result.Content[0])
	}
	return text.Text
}

func strPtr(value string) *string { return &value }
