package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openconvo/openconvo/internal/embeddings"
)

type fakeEmbeddingSettings struct {
	view    embeddings.SettingsView
	saveErr error
}

func (f *fakeEmbeddingSettings) GetSettings(context.Context) (embeddings.SettingsView, error) {
	return f.view, nil
}

func (f *fakeEmbeddingSettings) SaveSettings(_ context.Context, settings embeddings.Settings) (embeddings.SettingsView, error) {
	if f.saveErr != nil {
		return embeddings.SettingsView{}, f.saveErr
	}
	f.view.Settings = settings
	return f.view, nil
}

func TestEmbeddingSettingsEndpoints(t *testing.T) {
	fake := &fakeEmbeddingSettings{view: embeddings.SettingsView{
		Settings:              embeddings.Preset(false),
		CredentialsConfigured: true,
		Source:                "environment",
		EligibleMessages:      12,
	}}
	handler := newTestHandler(Deps{Embeddings: fake})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/settings", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"eligible_messages":12`) {
		t.Fatalf("GET settings: %d %s", recorder.Code, recorder.Body.String())
	}

	body := `{"enabled":true,"provider":"openai","model":"text-embedding-3-small","dimensions":256,"input_version":"message-content-v1"}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/embeddings/settings", strings.NewReader(body)))
	if recorder.Code != http.StatusOK || !fake.view.Enabled {
		t.Fatalf("PUT settings: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestEmbeddingSettingsRejectUnknownAndConfigurationErrors(t *testing.T) {
	fake := &fakeEmbeddingSettings{view: embeddings.SettingsView{Settings: embeddings.Preset(false)}}
	handler := newTestHandler(Deps{Embeddings: fake})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/embeddings/settings", strings.NewReader(`{"unknown":true}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", recorder.Code)
	}

	fake.saveErr = errors.Join(embeddings.ErrNotConfigured, errors.New("set OPENAI_API_KEY"))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/embeddings/settings",
		strings.NewReader(`{"enabled":true,"provider":"openai","model":"text-embedding-3-small","dimensions":256,"input_version":"message-content-v1"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("configuration error status = %d", recorder.Code)
	}
}
