package updates

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckFindsCompatibleUpdateAndCachesIt(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "tag_name":"v1.4.2",
            "html_url":"https://github.com/openconvo/openconvo/releases/tag/v1.4.2",
            "published_at":"2026-08-20T12:00:00Z"
        }`))
	}))
	defer server.Close()

	checker := New("1.3.0")
	checker.endpoint = server.URL
	checker.now = func() time.Time { return time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC) }

	for range 2 {
		status, err := checker.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !status.UpdateAvailable || !status.CommandUpgradeAllowed || status.LatestVersion != "1.4.2" {
			t.Fatalf("status = %+v", status)
		}
		if status.UpgradeCommand != "./scripts/upgrade.sh 1.4.2" {
			t.Errorf("upgrade command = %q", status.UpgradeCommand)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one cached request", requests)
	}
}

func TestCheckRequiresManualUpgradeAcrossCompatibilityBoundary(t *testing.T) {
	server := releaseServer(t, "v0.5.0")
	defer server.Close()
	checker := New("0.4.9")
	checker.endpoint = server.URL

	status, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.UpdateAvailable || status.CommandUpgradeAllowed || status.Reason != "manual-upgrade-required" {
		t.Fatalf("status = %+v", status)
	}
}

func TestCheckHandlesDevelopmentBuild(t *testing.T) {
	server := releaseServer(t, "v1.2.3")
	defer server.Close()
	checker := New("5386c69-dirty")
	checker.endpoint = server.URL

	status, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Reason != "development-build" || status.UpdateAvailable || status.CommandUpgradeAllowed {
		t.Fatalf("status = %+v", status)
	}
}

func TestCheckRecognizesSemanticLookingDevelopmentBuilds(t *testing.T) {
	for _, current := range []string{"0.1.0-dev", "v1.2.3-4-gabc1234", "1.2.3-dirty"} {
		server := releaseServer(t, "v1.2.4")
		checker := New(current)
		checker.endpoint = server.URL

		status, err := checker.Check(context.Background())
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if status.Reason != "development-build" || status.UpdateAvailable {
			t.Errorf("current %q: status = %+v", current, status)
		}
	}
}

func TestSemanticVersionOrdering(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0-dev", "1.0.0", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"v0.4.9", "0.5.0", -1},
	}
	for _, test := range tests {
		left, err := parseVersion(test.left)
		if err != nil {
			t.Fatal(err)
		}
		right, err := parseVersion(test.right)
		if err != nil {
			t.Fatal(err)
		}
		if got := left.compare(right); got != test.want {
			t.Errorf("compare(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func releaseServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://example.test/release"}`))
	}))
}
