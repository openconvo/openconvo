package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
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

func TestSearchMessagesToolRoutesFTSAndReturnsReducedResults(t *testing.T) {
	created := time.Date(2026, 8, 20, 11, 12, 13, 0, time.FixedZone("test", 8*60*60))
	keyword := &fakeSearch{page: archive.SearchPage{
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
	}}
	client := connectClient(t, Deps{Keyword: keyword})

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
	text := resultText(t, result)
	if strings.Contains(text, "private-actor-id") || strings.Contains(text, "avatar") {
		t.Errorf("result exposed actor fields that search clients do not need: %s", text)
	}
}

func TestSearchMessagesToolRoutesSemanticSearch(t *testing.T) {
	keyword := &fakeSearch{}
	semantic := &fakeSearch{page: archive.SearchPage{Results: []archive.SearchResult{}}}
	client := connectClient(t, Deps{Keyword: keyword, Semantic: semantic})

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
			keyword := &fakeSearch{}
			client := connectClient(t, Deps{Keyword: keyword})
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

func TestSearchMessagesToolExplainsSemanticState(t *testing.T) {
	semantic := &fakeSearch{err: embeddings.ErrDisabled}
	client := connectClient(t, Deps{Keyword: &fakeSearch{}, Semantic: semantic})
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

func TestServerAdvertisesOnlyReadOnlySearch(t *testing.T) {
	client := connectClient(t, Deps{Keyword: &fakeSearch{}})
	listed, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "search_messages" {
		t.Fatalf("tools = %+v", listed.Tools)
	}
	tool := listed.Tools[0]
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Fatalf("annotations = %+v", tool.Annotations)
	}
	if !strings.Contains(tool.Description, "non-deleted") {
		t.Errorf("description does not state deletion behavior: %q", tool.Description)
	}
}

func TestUnexpectedSearchErrorsAreNotExposed(t *testing.T) {
	client := connectClient(t, Deps{Keyword: &fakeSearch{err: errors.New("postgres password secret")}})
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
	server := New(Deps{Keyword: &fakeSearch{}}, "test")
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
	handler, err := NewHTTPHandler(New(Deps{Keyword: &fakeSearch{}}, "test"), token, nil)
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
	keyword := &fakeSearch{page: archive.SearchPage{Results: []archive.SearchResult{{
		MessageID: "message-http", ChannelID: testChannelID,
		ChannelName: "general", CommunityName: "OpenConvo",
		SourceCreatedAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		Excerpt:         "remote result",
	}}}}
	handler, err := NewHTTPHandler(New(Deps{Keyword: keyword}, "test"), token, nil)
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
