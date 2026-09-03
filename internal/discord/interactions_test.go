package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/openconvo/openconvo/internal/archive"
)

type interactionSaver struct {
	created   bool
	err       error
	source    string
	channelID string
	messageID string
}

func (s *interactionSaver) CreateBookmarkBySourceIdentity(_ context.Context, source, channelID, messageID string) (archive.Bookmark, bool, error) {
	s.source, s.channelID, s.messageID = source, channelID, messageID
	return archive.Bookmark{ID: "bookmark-1"}, s.created, s.err
}

func TestRegisterArchiveCommand(t *testing.T) {
	var method, authorization string
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		authorization = r.Header.Get("Authorization")
		decodeErr = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client := NewClient("secret").WithBaseURL(server.URL)
	if err := client.RegisterArchiveCommand(context.Background(), "app-1"); err != nil {
		t.Fatal(err)
	}
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if method != http.MethodPost || authorization != "Bot secret" {
		t.Fatalf("request = %s auth %q", method, authorization)
	}
	if body["name"] != archiveCommandName || body["type"] != float64(applicationCommandTypeMessage) ||
		body["default_member_permissions"] != permissionManageGuild ||
		!reflect.DeepEqual(body["integration_types"], []any{float64(applicationIntegrationGuildInstall)}) ||
		!reflect.DeepEqual(body["contexts"], []any{float64(interactionContextGuild)}) {
		t.Errorf("command body = %#v", body)
	}
}

func TestArchiveMessageInteraction(t *testing.T) {
	var callbackAuth string
	var decodeErr error
	var response struct {
		Type int `json:"type"`
		Data struct {
			Content string `json:"content"`
			Flags   int    `json:"flags"`
		} `json:"data"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions/interaction-1/token-1/callback" {
			http.NotFound(w, r)
			return
		}
		callbackAuth = r.Header.Get("Authorization")
		decodeErr = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&response)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	saver := &interactionSaver{created: true}
	source := NewSource("secret")
	source.client.WithBaseURL(server.URL)
	raw := json.RawMessage(`{
		"id":"interaction-1","type":2,"token":"token-1","channel_id":"channel-1",
		"data":{"name":"Archive","type":3,"target_id":"message-1"}
	}`)
	if err := source.applyEvent(context.Background(), SourceDeps{Bookmarks: saver}, GatewayEvent{
		Type: "INTERACTION_CREATE", Data: raw,
	}); err != nil {
		t.Fatal(err)
	}
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if saver.source != archive.SourceDiscord || saver.channelID != "channel-1" || saver.messageID != "message-1" {
		t.Errorf("save identity = %q %q %q", saver.source, saver.channelID, saver.messageID)
	}
	if callbackAuth != "" {
		t.Errorf("interaction callback sent bot authorization: %q", callbackAuth)
	}
	if response.Type != interactionResponseChannelMessage || response.Data.Flags != messageFlagEphemeral ||
		!strings.Contains(response.Data.Content, "Saved") {
		t.Errorf("response = %+v", response)
	}
}

func TestArchiveMessageInteractionRejectsUnarchivedMessage(t *testing.T) {
	var content string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response struct {
			Data struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&response)
		content = response.Data.Content
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	source := NewSource("secret")
	source.client.WithBaseURL(server.URL)
	saver := &interactionSaver{err: archive.ErrNotFound}
	err := source.handleArchiveInteraction(context.Background(), saver, json.RawMessage(`{
		"id":"i","type":2,"token":"t","channel_id":"disabled-channel",
		"data":{"name":"Archive","type":3,"target_id":"not-archived"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "not in an enabled") {
		t.Errorf("response content = %q", content)
	}
}
