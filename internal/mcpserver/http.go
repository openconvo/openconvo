package mcpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	minimumHTTPTokenBytes = 32
	maximumHTTPBodyBytes  = 1 << 20
)

// NewHTTPHandler exposes server over stateless Streamable HTTP. Every request
// requires the dedicated bearer token; browser administrator sessions are a
// separate trust boundary and are intentionally not accepted here.
func NewHTTPHandler(server *mcp.Server, token string, logger *slog.Logger) (http.Handler, error) {
	if server == nil {
		return nil, errors.New("MCP HTTP server is required")
	}
	token = strings.TrimSpace(token)
	if len(token) < minimumHTTPTokenBytes {
		return nil, errors.New("MCP HTTP bearer token must be at least 32 characters")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			Logger:       logger.With("component", "mcp", "transport", "http"),
		},
	)

	// Native MCP clients omit Origin. A browser-originated cross-site request is
	// refused even if it somehow obtains a token, and the small body ceiling
	// prevents the SDK's JSON decoder from becoming an unbounded input surface.
	crossOrigin := http.NewCrossOriginProtection().Handler(streamable)
	bounded := http.MaxBytesHandler(crossOrigin, maximumHTTPBodyBytes)
	return requireBearer(token, bounded), nil
}

func requireBearer(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		credential, ok := bearerCredential(r.Header.Get("Authorization"))
		got := sha256.Sum256([]byte(credential))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="openconvo-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerCredential(header string) (string, bool) {
	scheme, credential, ok := strings.Cut(strings.TrimSpace(header), " ")
	credential = strings.TrimSpace(credential)
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return "", false
	}
	return credential, true
}
