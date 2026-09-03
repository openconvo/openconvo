// Package app wires configuration, database, storage, jobs, sources
// and the HTTP server into the single long-running OpenConvo process.
// OpenConvo is deliberately one process: API, frontend, ingestion and
// background jobs all live in this binary (see docs/architecture.md).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/attachments"
	"github.com/openconvo/openconvo/internal/backups"
	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/embeddings"
	httpserver "github.com/openconvo/openconvo/internal/http"
	"github.com/openconvo/openconvo/internal/ingest"
	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/mcpserver"
	"github.com/openconvo/openconvo/internal/storage"
	"github.com/openconvo/openconvo/internal/syncer"
	"github.com/openconvo/openconvo/internal/updates"
	"github.com/openconvo/openconvo/internal/version"
	"github.com/openconvo/openconvo/internal/web"
)

// Run starts OpenConvo and blocks until ctx is cancelled or a fatal
// error occurs. Startup order: database → migrations → storage → jobs →
// HTTP. Shutdown is graceful: HTTP drains, in-flight jobs finish.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	startedAt := time.Now().UTC()
	logger.Info("starting openconvo", "version", version.Version, "commit", version.Commit)

	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	if err := cfg.RequireAdminPassword(); err != nil {
		return err
	}
	authenticator, err := httpserver.NewAuthenticator(cfg.AdminPassword)
	if err != nil {
		return fmt.Errorf("initialize administrator authentication: %w", err)
	}

	// Database.
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	logger.Info("database connected")

	if cfg.AutoMigrate {
		applied, err := database.Migrate(ctx, pool)
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		if applied > 0 {
			logger.Info("migrations applied", "count", applied)
		}
	}
	schemaVersion, err := database.SchemaVersion(ctx, pool)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	logger.Info("schema ready", "version", schemaVersion)

	// Attachment storage.
	var blobs storage.Store
	switch cfg.StorageDriver {
	case config.StorageDriverFilesystem:
		blobs, err = storage.NewFilesystem(cfg.StoragePath)
	case config.StorageDriverS3:
		storageCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		blobs, err = storage.NewS3(storageCtx, storage.S3Options{
			Endpoint:       cfg.S3Endpoint,
			Region:         cfg.S3Region,
			Bucket:         cfg.S3Bucket,
			AccessKey:      cfg.S3AccessKey,
			SecretKey:      cfg.S3SecretKey,
			SessionToken:   cfg.S3SessionToken,
			ForcePathStyle: cfg.S3ForcePathStyle,
		})
		cancel()
	default:
		return fmt.Errorf("unsupported storage driver %q", cfg.StorageDriver)
	}
	if err != nil {
		return err
	}
	logger.Info("storage ready", "component", "storage", "driver", cfg.StorageDriver)

	store := archive.New(pool)

	// Background jobs.
	queue := jobs.NewQueue(pool)
	worker := jobs.NewWorker(queue, logger)
	backupService := backups.New(pool, queue, backups.Options{
		Defaults: backups.Settings{
			Enabled:        cfg.BackupEnabled,
			Provider:       cfg.BackupProvider,
			Endpoint:       cfg.BackupS3Endpoint,
			Region:         cfg.BackupS3Region,
			Bucket:         cfg.BackupS3Bucket,
			Prefix:         cfg.BackupS3Prefix,
			ForcePathStyle: cfg.BackupS3ForcePathStyle,
			IntervalHours:  cfg.BackupIntervalHours,
			RetentionCount: cfg.BackupRetentionCount,
		},
		Credentials: backups.Credentials{
			AccessKey:    cfg.BackupS3AccessKey,
			SecretKey:    cfg.BackupS3SecretKey,
			SessionToken: cfg.BackupS3SessionToken,
		},
		DatabaseURL: cfg.DatabaseURL,
		PGDumpPath:  cfg.BackupPGDumpPath,
	}, logger)
	backupWorker := jobs.NewWorker(queue, logger).WithConcurrency(1)
	backupService.RegisterHandlers(backupWorker)
	embeddingService := embeddings.New(pool, queue, embeddings.Options{
		Defaults: embeddings.Preset(cfg.EmbeddingsEnabled),
		APIKey:   cfg.OpenAIAPIKey,
	}, logger)
	if err := embeddingService.Initialize(ctx); err != nil {
		return fmt.Errorf("initialize embeddings: %w", err)
	}
	embeddingWorker := jobs.NewWorker(queue, logger).WithConcurrency(2)
	embeddingService.RegisterHandlers(embeddingWorker)

	// The single archive write path, shared by live events, backfill and
	// reconciliation.
	ingester := ingest.New(store, logger).WithMessageStored(embeddingService.ScheduleMessage)

	// One Discord client, so rate-limit accounting stays in one place.
	var source *discord.Source
	if cfg.DiscordConfigured() {
		source = discord.NewSource(cfg.DiscordToken)
	}

	// Attachment pipeline. The refresher is the Discord client when one
	// is configured; without it, downloading is impossible and stays
	// off, but reclamation still runs — it enforces deletion, which is
	// not optional.
	var refresher attachments.URLRefresher
	if source != nil {
		refresher = source.Client()
	}
	downloader := attachments.New(store, blobs, queue, refresher, attachments.Options{
		Enabled:  cfg.AttachmentsEnabled,
		MaxBytes: cfg.AttachmentMaxBytes,
	}, logger)

	// A second worker so a large backlog of downloads cannot occupy
	// every slot and starve Discord synchronization.
	attachmentWorker := jobs.NewWorker(queue, logger).WithConcurrency(2)
	downloader.RegisterHandlers(attachmentWorker)

	// Everything long-running shares one cancellable context so
	// shutdown drains gateway, sync, worker and HTTP together.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup

	assets, built := web.Assets()
	if !built {
		assets = nil
		logger.Warn("frontend assets not embedded in this build; serving fallback page")
	}
	httpDeps := httpserver.Deps{
		Logger:        logger,
		Auth:          authenticator,
		WebAssets:     assets,
		CheckDatabase: func(ctx context.Context) error { return database.Ping(ctx, pool) },
		// downloader.Enabled(), not cfg.AttachmentsEnabled: with
		// downloads switched on and no Discord token nothing can be
		// downloaded, and status must say that rather than promise it.
		Status:         statusProvider(cfg, pool, store, source, downloader.Enabled(), startedAt, logger),
		Archive:        store,
		Blobs:          blobs,
		Backups:        backupService,
		Embeddings:     embeddingService,
		SemanticSearch: embeddingService,
		Updates:        updates.New(version.Version),
	}
	if cfg.MCPHTTPEnabled {
		// Remote MCP is a distinct reader boundary. Its pool defaults every
		// PostgreSQL session to read-only, so the HTTP process's write-capable
		// archive pool is never handed to a model-controlled tool.
		mcpPool, err := database.ConnectReadOnly(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("connect remote MCP reader to database: %w", err)
		}
		defer mcpPool.Close()
		mcpSemantic := embeddings.New(mcpPool, nil, embeddings.Options{
			Defaults: embeddings.Preset(cfg.EmbeddingsEnabled),
			APIKey:   cfg.OpenAIAPIKey,
		}, logger)
		mcpProtocol := mcpserver.New(mcpserver.Deps{
			Keyword: archive.New(mcpPool), Semantic: mcpSemantic, Logger: logger,
		}, version.Version)
		httpDeps.MCP, err = mcpserver.NewHTTPHandler(mcpProtocol, cfg.MCPToken, logger)
		if err != nil {
			return fmt.Errorf("initialize remote MCP endpoint: %w", err)
		}
		logger.Info("remote MCP enabled", "component", "mcp", "path", "/mcp")
	}

	// Discord source: live gateway sync plus the backfill/reconcile
	// coordinator. A valid token proves itself by reaching READY, so
	// there is no separate credential check.
	if source != nil {
		// Named "syn" because this file already uses the stdlib "sync".
		syn := syncer.New(store, queue, source.Client(), ingester, logger)
		syn.RegisterHandlers(worker)
		httpDeps.OnChannelToggle = syn.ChannelToggled

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := source.Run(runCtx, discord.SourceDeps{
				Ingester:      ingester,
				ApplicationID: cfg.DiscordApplicationID,
				Bookmarks:     store,
				// A session that could not resume may have missed
				// events: reconcile everything that is synced.
				OnResync: func() { syn.ReconcileAll(context.WithoutCancel(runCtx)) },
				Logger:   logger,
			})
			var fatal *discord.FatalGatewayError
			if errors.As(err, &fatal) {
				// Unrecoverable (bad token, missing intent): keep serving
				// the archive, but tell the operator loudly.
				logger.Error("discord connection permanently failed", "component", "discord", "error", fatal)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			syn.Run(runCtx)
		}()
	} else {
		logger.Info("discord not configured; archival is idle until DISCORD_TOKEN is set", "component", "discord")
	}

	// HTTP server.
	if warning := cfg.BindWarning(runningInContainer()); warning != "" {
		logger.Warn(warning, "component", "http")
	}
	server := httpserver.New(
		httpserver.Config{Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)},
		httpDeps,
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		attachmentWorker.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		backupWorker.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		embeddingWorker.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		backupService.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		embeddingService.Run(runCtx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		downloader.Run(runCtx)
	}()

	err = server.Run(runCtx)
	cancel()
	wg.Wait()
	logger.Info("openconvo stopped")
	return err
}

// statusProvider assembles the system status document. It degrades
// rather than fails: a database outage yields connected=false, not an
// unusable endpoint.
func statusProvider(cfg config.Config, pool *pgxpool.Pool, store *archive.Store, source *discord.Source, attachmentsEnabled bool, startedAt time.Time, logger *slog.Logger) func(context.Context) (httpserver.StatusResponse, error) {
	return func(ctx context.Context) (httpserver.StatusResponse, error) {
		storagePath := ""
		if cfg.StorageDriver == config.StorageDriverFilesystem {
			storagePath = cfg.StoragePath
		}
		resp := httpserver.StatusResponse{
			Version:   version.Get(),
			StartedAt: startedAt,
			Storage: httpserver.StorageStatus{
				Driver: cfg.StorageDriver,
				Path:   storagePath,
			},
			Discord: httpserver.DiscordStatus{
				Configured:    cfg.DiscordConfigured(),
				ApplicationID: cfg.DiscordApplicationID,
			},
		}
		if source != nil {
			status := source.Status()
			resp.Discord.Connected = status.Connected
			resp.Discord.BotUsername = status.BotUsername
			resp.Discord.LastError = status.LastError
		}

		if err := database.Ping(ctx, pool); err != nil {
			resp.Database = httpserver.DatabaseStatus{Connected: false, Error: "unreachable"}
			return resp, nil
		}
		resp.Database.Connected = true

		if v, err := database.SchemaVersion(ctx, pool); err == nil {
			resp.Database.SchemaVersion = v
		}

		counts, err := store.Counts(ctx)
		if err != nil {
			logger.Warn("status: counting archive failed", "error", err)
			return resp, nil
		}
		resp.Counts = &httpserver.CountsStatus{
			Communities: counts.Communities,
			Channels:    counts.Channels,
			Messages:    counts.Messages,
			Attachments: counts.Attachments,
		}

		if stats, err := store.AttachmentStats(ctx); err == nil {
			resp.Attachments = &httpserver.AttachmentsStatus{
				Enabled:     attachmentsEnabled,
				Stored:      stats.Stored,
				Pending:     stats.Pending,
				Failed:      stats.Failed,
				StoredBytes: stats.StoredBytes,
			}
		} else {
			logger.Warn("status: counting attachments failed", "error", err)
		}
		return resp, nil
	}
}

// runningInContainer reports whether this process is inside a Docker
// container. The marker file is Docker's own and long-standing; a wrong
// answer here only decides whether one configuration warning is logged.
func runningInContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}
