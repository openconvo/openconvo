package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshAttachmentURLs(t *testing.T) {
	var gotBody struct {
		AttachmentURLs []string `json:"attachment_urls"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/attachments/refresh-urls" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bot tok" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed_urls":[
			{"original":"https://cdn/a.png","refreshed":"https://cdn/a.png?ex=1&hm=2"}
		]}`))
	}))
	defer server.Close()

	client := NewClient("tok").WithBaseURL(server.URL)
	got, err := client.RefreshAttachmentURLs(context.Background(), []string{"https://cdn/a.png"})
	if err != nil {
		t.Fatalf("RefreshAttachmentURLs: %v", err)
	}
	if len(gotBody.AttachmentURLs) != 1 || gotBody.AttachmentURLs[0] != "https://cdn/a.png" {
		t.Errorf("request body = %+v", gotBody)
	}
	if got["https://cdn/a.png"] != "https://cdn/a.png?ex=1&hm=2" {
		t.Errorf("refreshed = %v", got)
	}
}

// More than the batch limit must not be silently truncated.
func TestRefreshAttachmentURLsRejectsOversizeBatch(t *testing.T) {
	client := NewClient("tok")
	urls := make([]string, RefreshURLsBatchLimit+1)
	for i := range urls {
		urls[i] = "https://cdn/x.png"
	}
	_, err := client.RefreshAttachmentURLs(context.Background(), urls)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("err = %v, want a batch-limit error", err)
	}
}

func TestRefreshAttachmentURLsEmpty(t *testing.T) {
	client := NewClient("tok")
	got, err := client.RefreshAttachmentURLs(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, err %v; want an empty result and no request", got, err)
	}
}
