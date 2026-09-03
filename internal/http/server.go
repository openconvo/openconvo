// Package http serves the OpenConvo API and the archive frontend.
//
// The API lives under /api/v1 and is versioned from day one. The
// compiled React frontend is embedded in the binary and served from
// the same process: one port, one service, nothing else to deploy.
package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/backups"
	"github.com/openconvo/openconvo/internal/embeddings"
	"github.com/openconvo/openconvo/internal/updates"
)

// Config is the HTTP server configuration.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
}

// ArchiveAPI is the read and channel-selection surface exposed to the HTTP
// layer. *archive.Store satisfies it; tests use fakes.
type ArchiveAPI interface {
	ListCommunities(ctx context.Context) ([]archive.Community, error)
	ListChannels(ctx context.Context, communityID string) ([]archive.Channel, error)
	ListArchiveChannels(ctx context.Context) ([]archive.ArchiveChannel, error)
	GetChannel(ctx context.Context, channelID string) (archive.Channel, bool, error)
	GetArchiveChannel(ctx context.Context, channelID string) (archive.ArchiveChannel, bool, error)
	SetChannelArchiveEnabled(ctx context.Context, channelID string, enabled bool) error
	SyncOverview(ctx context.Context) ([]archive.SyncOverviewRow, error)
	ListMessages(ctx context.Context, channelID, before string, limit int) (archive.MessagePage, error)
	GetMessageContext(ctx context.Context, messageID string, beforeCount, afterCount int) (archive.MessageContext, bool, error)
	SearchMessages(ctx context.Context, params archive.SearchParams) (archive.SearchPage, error)
	GetStoredAttachment(ctx context.Context, attachmentID string) (archive.StoredAttachment, bool, error)
	ListBookmarks(ctx context.Context, filter archive.BookmarkFilter) ([]archive.Bookmark, error)
	CreateBookmark(ctx context.Context, in archive.BookmarkUpsert) (archive.Bookmark, bool, error)
	UpdateBookmark(ctx context.Context, bookmarkID string, in archive.BookmarkUpsert) (archive.Bookmark, error)
	DeleteBookmark(ctx context.Context, bookmarkID string) error
}

// BlobStore is the read side of content-addressed attachment storage.
type BlobStore interface {
	Open(ctx context.Context, sha256hex string) (io.ReadCloser, error)
}

// BackupAPI is the authenticated database-backup administration surface.
type BackupAPI interface {
	GetSettings(context.Context) (backups.SettingsView, error)
	SaveSettings(context.Context, backups.Settings) (backups.SettingsView, error)
	RequestBackup(context.Context, string) (backups.Backup, bool, error)
	ListBackups(context.Context, int) ([]backups.Backup, error)
	OpenBackup(context.Context, string) (backups.Backup, io.ReadCloser, error)
}

// EmbeddingAPI is the authenticated, secret-free configuration surface for
// the optional derived semantic index.
type EmbeddingAPI interface {
	GetSettings(context.Context) (embeddings.SettingsView, error)
	SaveSettings(context.Context, embeddings.Settings) (embeddings.SettingsView, error)
}

// SemanticSearchAPI is kept separate from settings so handlers and tests can
// depend on only the derived read operation they use.
type SemanticSearchAPI interface {
	SearchMessages(context.Context, archive.SearchParams) (archive.SearchPage, error)
}

// UpdateChecker is the read-only release-checking surface. Applying an update
// deliberately remains outside the application process.
type UpdateChecker interface {
	Check(context.Context) (updates.Status, error)
}

// Deps are the dependencies handlers need, injected as the small interfaces
// and functions declared above so the HTTP layer stays decoupled from the
// database and app wiring.
type Deps struct {
	Logger *slog.Logger
	// Auth protects every administrator API route. It is required: the
	// archive is private, nothing else can tell an administrator from the
	// internet, so without it the whole /api/ subtree refuses to answer.
	Auth *Authenticator
	// CheckDatabase pings the database (nil error = healthy).
	CheckDatabase func(ctx context.Context) error
	// Status assembles the system status document.
	Status func(ctx context.Context) (StatusResponse, error)
	// WebAssets is the built frontend, or nil when not built.
	WebAssets fs.FS
	// Archive backs the discovery and selection routes; when nil they
	// answer 503 rather than existing in a half-working state.
	Archive ArchiveAPI
	// Blobs supplies attachment bytes for authenticated downloads.
	Blobs BlobStore
	// Backups manages scheduled logical dumps and their downloads.
	Backups BackupAPI
	// Embeddings owns the optional derived message-vector index.
	Embeddings EmbeddingAPI
	// SemanticSearch queries the active derived vector generation.
	SemanticSearch SemanticSearchAPI
	// MCP is the independently authenticated Streamable HTTP endpoint. It is
	// nil unless the operator explicitly enables remote MCP access.
	MCP http.Handler
	// Updates checks the latest published OpenConvo release.
	Updates UpdateChecker
	// OnChannelToggle runs after a successful selection change (sync
	// scheduling). Nil means the toggle only changes metadata.
	OnChannelToggle func(ctx context.Context, channelID string, enabled bool) error
}

// Server is the OpenConvo HTTP server.
type Server struct {
	http   *http.Server
	logger *slog.Logger
}

// New assembles the routing table and middleware chain.
func New(cfg Config, deps Deps) *Server {
	logger := deps.Logger.With("component", "http")
	// Handlers get the enriched logger too.
	deps.Logger = logger

	exposure := newExposureMonitor(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth(deps))
	if deps.MCP != nil {
		// This route deliberately sits outside /api/'s browser-session gate:
		// remote MCP clients authenticate with their own bearer credential.
		mux.Handle("/mcp", deps.MCP)
	} else {
		// Without an explicit route the SPA fallback would answer /mcp with HTML,
		// making a disabled protocol endpoint look mysteriously malformed.
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, http.StatusNotFound, "not found")
		})
	}

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/system/status", handleStatus(deps, exposure))
	api.HandleFunc("GET /api/v1/system/sync", handleSyncOverview(deps))
	api.HandleFunc("GET /api/v1/system/update", handleUpdate(deps))
	api.HandleFunc("GET /api/v1/communities", handleListCommunities(deps))
	api.HandleFunc("GET /api/v1/communities/{id}/channels", handleListChannels(deps))
	api.HandleFunc("GET /api/v1/channels", handleListArchiveChannels(deps))
	api.HandleFunc("GET /api/v1/channels/{id}/messages", handleListMessages(deps))
	api.HandleFunc("PUT /api/v1/channels/{id}/archive", handleToggleChannel(deps))
	api.HandleFunc("GET /api/v1/messages/{id}", handleMessageContext(deps))
	api.HandleFunc("GET /api/v1/search", handleSearch(deps))
	api.HandleFunc("GET /api/v1/bookmarks", handleListBookmarks(deps))
	api.HandleFunc("POST /api/v1/bookmarks", handleCreateBookmark(deps))
	api.HandleFunc("PUT /api/v1/bookmarks/{id}", handleUpdateBookmark(deps))
	api.HandleFunc("DELETE /api/v1/bookmarks/{id}", handleDeleteBookmark(deps))
	api.HandleFunc("GET /api/v1/attachments/{id}/content", handleAttachmentContent(deps))
	api.HandleFunc("HEAD /api/v1/attachments/{id}/content", handleAttachmentContent(deps))
	api.HandleFunc("GET /api/v1/backups/settings", handleBackupSettings(deps))
	api.HandleFunc("PUT /api/v1/backups/settings", handleSaveBackupSettings(deps))
	api.HandleFunc("GET /api/v1/backups", handleListBackups(deps))
	api.HandleFunc("POST /api/v1/backups", handleCreateBackup(deps))
	api.HandleFunc("GET /api/v1/backups/{id}/content", handleBackupContent(deps))
	api.HandleFunc("HEAD /api/v1/backups/{id}/content", handleBackupContent(deps))
	api.HandleFunc("GET /api/v1/embeddings/settings", handleEmbeddingSettings(deps))
	api.HandleFunc("PUT /api/v1/embeddings/settings", handleSaveEmbeddingSettings(deps))
	// Unknown API routes must return JSON, not the SPA shell.
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	// Everything under /api/ goes through one gate. The three session
	// routes are registered on the outer mux because logging in cannot
	// require being logged in; each carries its own same-origin check in
	// place of the one Protect would have applied. Nothing else belongs
	// out here — a literal pattern wins over the "/api/" subtree, so a
	// route registered on mux instead of api silently escapes the gate.
	protectedAPI := unauthenticatedAPI(logger)
	if deps.Auth != nil {
		mux.HandleFunc("GET /api/v1/auth/session", handleAuthStatus(deps.Auth))
		mux.HandleFunc("POST /api/v1/auth/session", handleLogin(deps.Auth))
		mux.HandleFunc("DELETE /api/v1/auth/session", handleLogout(deps.Auth))
		protectedAPI = deps.Auth.Protect(api)
	}
	mux.Handle("/api/", protectedAPI)
	mux.Handle("/", SPAHandler(deps.WebAssets))

	handler := requestID(logging(logger, recoverer(logger, exposure.middleware(mux))))

	return &Server{
		http: &http.Server{
			Addr:    cfg.Addr,
			Handler: handler,
			// These bound an API request: bodies are small and answers are
			// fast, so a slow one is a stuck or hostile client. The two
			// routes that stream a large file for minutes — backup and
			// attachment downloads — extend their own write deadline via
			// http.NewResponseController, which reaches the connection
			// because statusRecorder implements Unwrap.
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		},
		logger: logger,
	}
}

// unauthenticatedAPI answers the whole API when Deps.Auth is missing. There is
// no safe way to serve an archive of private conversations to callers nobody
// can identify, so a misassembled server refuses rather than publishes.
func unauthenticatedAPI(logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Error("refusing an API request: no authenticator was supplied", "path", r.URL.Path)
		w.Header().Set("Cache-Control", "no-store")
		writeError(w, http.StatusServiceUnavailable, "authentication is not configured")
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.http.Addr, err)
	}
	s.logger.Info("http server listening", "addr", s.http.Addr)

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.logger.Info("http server stopped")
	return nil
}
