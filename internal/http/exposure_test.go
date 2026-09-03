package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicClientClassification(t *testing.T) {
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"the internet", "203.0.113.10", true},
		{"the internet over IPv6", "2001:db8::1", true},
		{"loopback", "127.0.0.1", false},
		{"loopback over IPv6", "::1", false},
		{"a home network", "192.168.1.20", false},
		{"a datacentre private network", "10.128.0.4", false},
		{"the other private range", "172.16.5.9", false},
		{"a unique-local IPv6 network", "fd00::5", false},
		// Tailscale and other WireGuard overlays hand out carrier-grade NAT
		// addresses. That traffic is already encrypted, and warning about it
		// would train operators to ignore the warning.
		{"a tailnet", "100.101.102.103", false},
		{"link-local autoconfiguration", "169.254.10.1", false},
		{"an unparseable forwarded value", "not-an-address", false},
		{"nothing at all", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := publicClient(tc.host); got != tc.want {
				t.Fatalf("publicClient(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestExposureFlagsPlainHTTPFromTheInternet(t *testing.T) {
	var logs bytes.Buffer
	monitor := newExposureMonitor(slog.New(slog.NewTextHandler(&logs, nil)))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:51000"
	monitor.observe(req)

	if !monitor.insecurePublicAccess() {
		t.Fatal("a public client served over plain HTTP was not recorded as exposed")
	}
	if !strings.Contains(logs.String(), "plain HTTP") {
		t.Fatalf("no warning was logged; got %q", logs.String())
	}

	// The warning is a deployment fact, not a per-request event: repeating it
	// for every request would bury the logs an operator needs to read.
	before := logs.Len()
	monitor.observe(req)
	if logs.Len() != before {
		t.Fatal("the exposure warning was logged more than once")
	}
}

func TestExposureStaysQuietWhenProperlyDeployed(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
	}{
		{
			name:       "a reverse proxy reporting HTTPS",
			remoteAddr: "127.0.0.1:44000",
			headers: map[string]string{
				"X-Forwarded-For":   "203.0.113.10",
				"X-Forwarded-Proto": "https",
			},
		},
		{
			name:       "a local reverse proxy with no forwarding headers",
			remoteAddr: "127.0.0.1:44000",
		},
		{
			name:       "an administrator on the same network",
			remoteAddr: "192.168.1.20:51000",
		},
		{
			name:       "a browser on this machine",
			remoteAddr: "127.0.0.1:51000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			monitor := newExposureMonitor(testLogger())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			monitor.observe(req)

			if monitor.insecurePublicAccess() {
				t.Fatal("a correctly deployed archive was reported as exposed")
			}
		})
	}
}

// A proxy that forwards the connection but not the scheme is the case no
// documentation catches: the archive looks fine in a browser over HTTPS while
// its own session cookie is never marked Secure.
func TestExposureFlagsAProxyThatOmitsTheScheme(t *testing.T) {
	monitor := newExposureMonitor(testLogger())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:44000"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	monitor.observe(req)

	if !monitor.insecurePublicAccess() {
		t.Fatal("a proxy forwarding without X-Forwarded-Proto was not recorded as exposed")
	}
}

func TestStatusReportsInsecurePublicAccess(t *testing.T) {
	handler := newTestHandler(Deps{
		Status: func(context.Context) (StatusResponse, error) { return StatusResponse{}, nil },
	})

	status := func(t *testing.T) StatusResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		req.RemoteAddr = "127.0.0.1:51000"
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body StatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	if status(t).InsecurePublicAccess {
		t.Fatal("a freshly started server already reports insecure public access")
	}

	exposed := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	exposed.RemoteAddr = "203.0.113.10:51000"
	handler.ServeHTTP(httptest.NewRecorder(), exposed)

	if !status(t).InsecurePublicAccess {
		t.Fatal("status did not report the archive being served over plain HTTP")
	}
}
