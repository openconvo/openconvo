package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/backups"
)

const testBackupUUID = "0198c0de-0000-4000-8000-00000000bacc"

type fakeBackups struct {
	settings backups.SettingsView
	items    []backups.Backup
	content  []byte
}

func (f *fakeBackups) GetSettings(context.Context) (backups.SettingsView, error) {
	return f.settings, nil
}

func (f *fakeBackups) SaveSettings(_ context.Context, settings backups.Settings) (backups.SettingsView, error) {
	f.settings = backups.SettingsView{Settings: settings, CredentialsConfigured: true, Source: "dashboard"}
	return f.settings, nil
}

func (f *fakeBackups) RequestBackup(context.Context, string) (backups.Backup, bool, error) {
	backup := backups.Backup{ID: testBackupUUID, Status: "pending", CreatedAt: time.Now().UTC()}
	f.items = append([]backups.Backup{backup}, f.items...)
	return backup, true, nil
}

func (f *fakeBackups) ListBackups(context.Context, int) ([]backups.Backup, error) {
	return f.items, nil
}

func (f *fakeBackups) OpenBackup(_ context.Context, id string) (backups.Backup, io.ReadCloser, error) {
	if id != testBackupUUID {
		return backups.Backup{}, nil, backups.ErrNotFound
	}
	backup := backups.Backup{
		ID: testBackupUUID, Status: "succeeded", Size: int64(len(f.content)),
		CreatedAt: time.Date(2026, 8, 20, 3, 4, 5, 0, time.UTC),
	}
	return backup, io.NopCloser(bytes.NewReader(f.content)), nil
}

func TestBackupSettingsAndRunEndpoints(t *testing.T) {
	fake := &fakeBackups{settings: backups.SettingsView{
		Settings: backups.Settings{
			Provider: "r2", Region: "auto", IntervalHours: 24, RetentionCount: 30,
		},
		CredentialsConfigured: true,
		Source:                "environment",
	}}
	handler := newTestHandler(Deps{Backups: fake})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backups/settings", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"credentials_configured":true`) {
		t.Fatalf("GET settings: %d %s", rec.Code, rec.Body.String())
	}

	body := `{"enabled":true,"provider":"r2","endpoint":"https://account.r2.cloudflarestorage.com","region":"auto","bucket":"archive","prefix":"db","force_path_style":false,"interval_hours":12,"retention_count":14}`
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/backups/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK || fake.settings.Bucket != "archive" || fake.settings.IntervalHours != 12 {
		t.Fatalf("PUT settings: %d %s settings=%+v", rec.Code, rec.Body.String(), fake.settings)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/backups", nil))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), testBackupUUID) {
		t.Fatalf("POST backup: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testBackupUUID) {
		t.Fatalf("GET backups: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBackupSettingsRejectUnknownFields(t *testing.T) {
	handler := newTestHandler(Deps{Backups: &fakeBackups{}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/backups/settings", strings.NewReader(`{"unknown":true}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestBackupDownload(t *testing.T) {
	fake := &fakeBackups{content: []byte("database dump bytes")}
	handler := newTestHandler(Deps{Backups: fake})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backups/"+testBackupUUID+"/content", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "database dump bytes" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body.String())
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.Contains(disposition, "openconvo-db-20260820T030405Z.dump") {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("security headers = %v", rec.Header())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/v1/backups/"+testBackupUUID+"/content", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backups/not-a-uuid/content", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed ID: %d", rec.Code)
	}
}

func TestBackupRoutesWithoutDependency(t *testing.T) {
	handler := newTestHandler(Deps{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/backups", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
