package discordtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestFakeRESTAndGateway(t *testing.T) {
	s := New(t)
	s.AddGuild("g1", "Test Guild", json.RawMessage(`{"id":"c1","guild_id":"g1","type":0,"name":"general"}`))
	msgs := make([]json.RawMessage, 0, 150)
	for i := 150; i >= 1; i-- { // newest first: ids 150..1
		msgs = append(msgs, json.RawMessage(fmt.Sprintf(`{"id":"%d","channel_id":"c1","content":"m%d","timestamp":"2026-01-01T00:00:00Z","author":{"id":"u1","username":"u"}}`, i, i)))
	}
	s.SetMessages("c1", msgs)

	// REST: pagination honors before + limit.
	ctx := context.Background()
	resp := restGet(t, s.BaseURL()+"/channels/c1/messages?limit=100")
	var page []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 100 || page[0].ID != "150" || page[99].ID != "51" {
		t.Fatalf("page1: len=%d first=%s last=%s", len(page), page[0].ID, page[len(page)-1].ID)
	}
	resp = restGet(t, s.BaseURL()+"/channels/c1/messages?limit=100&before=51")
	if err := json.Unmarshal(resp, &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 50 || page[0].ID != "50" {
		t.Fatalf("page2: len=%d first=%s", len(page), page[0].ID)
	}

	// Gateway: HELLO → IDENTIFY → READY → dispatch flows.
	conn, _, err := websocket.Dial(ctx, s.GatewayURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	read := func() map[string]any {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		_ = json.Unmarshal(data, &p)
		return p
	}
	hello := read()
	if hello["op"].(float64) != 10 {
		t.Fatalf("expected HELLO, got %v", hello)
	}
	identify := `{"op":2,"d":{"token":"t","intents":34305,"properties":{"os":"test","browser":"openconvo","device":"openconvo"}}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(identify)); err != nil {
		t.Fatal(err)
	}
	ready := read()
	if ready["t"] != "READY" {
		t.Fatalf("expected READY, got %v", ready)
	}
	s.WaitForSession(t)
	s.Dispatch(t, "MESSAGE_CREATE", map[string]any{"id": "1", "channel_id": "c1"})
	ev := read()
	if ev["t"] != "MESSAGE_CREATE" {
		t.Fatalf("expected MESSAGE_CREATE, got %v", ev)
	}
	if s.Identifies() != 1 {
		t.Errorf("identifies = %d", s.Identifies())
	}
}

// TestWaitForSessionAfterEstablished guards the helper's contract: a
// session that established BEFORE the call must still satisfy it, or
// every gateway test becomes a race.
func TestWaitForSessionAfterEstablished(t *testing.T) {
	s := New(t)
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, s.GatewayURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err != nil { // HELLO
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"op":2,"d":{"token":"t"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(readCtx); err != nil { // READY
		t.Fatal(err)
	}
	// The session is already up; WaitForSession must return immediately.
	done := make(chan struct{})
	go func() { s.WaitForSession(t); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForSession blocked on an already-established session")
	}
}

func restGet(t *testing.T, url string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bot t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	return body
}
