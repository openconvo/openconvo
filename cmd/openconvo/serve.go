package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/openconvo/openconvo/internal/app"
	"github.com/openconvo/openconvo/internal/config"
)

func runServe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, cfg, logger)
}
