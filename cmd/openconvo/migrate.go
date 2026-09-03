package main

import (
	"context"
	"fmt"

	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
)

func runMigrate() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := database.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	version, err := database.SchemaVersion(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("applied %d migration(s); schema is at version %d\n", applied, version)
	return nil
}
