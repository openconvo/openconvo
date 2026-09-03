package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/embeddings"
	"github.com/openconvo/openconvo/internal/mcpserver"
	"github.com/openconvo/openconvo/internal/version"
)

const mcpHelp = `Usage: openconvo mcp

Expose one read-only search_messages tool to an MCP client over standard
input/output. DATABASE_URL is required. Semantic searches also use the
existing OpenConvo embedding settings and OPENAI_API_KEY.`

func runMCP(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	done, err := parseFlags(flags, mcpHelp, args)
	if done {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: openconvo mcp takes no arguments")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}

	// stdout belongs exclusively to the MCP JSON-RPC transport. Sending even
	// one log line there corrupts the connection, so this command has its own
	// stderr logger rather than using the server process logger.
	logger := mcpLogger(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.ConnectReadOnly(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	keyword := archive.New(pool)
	semantic := embeddings.New(pool, nil, embeddings.Options{
		Defaults: embeddings.Preset(cfg.EmbeddingsEnabled),
		APIKey:   cfg.OpenAIAPIKey,
	}, logger)
	server := mcpserver.New(mcpserver.Deps{
		Keyword: keyword, Semantic: semantic, Logger: logger,
	}, version.Version)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func mcpLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
