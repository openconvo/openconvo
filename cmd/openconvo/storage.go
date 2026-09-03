package main

import (
	"context"
	"fmt"
	"time"

	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/storage"
)

func openBlobStore(ctx context.Context, cfg config.Config) (storage.Store, error) {
	switch cfg.StorageDriver {
	case config.StorageDriverFilesystem:
		return storage.NewFilesystem(cfg.StoragePath)
	case config.StorageDriverS3:
		storageCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return storage.NewS3(storageCtx, storage.S3Options{
			Endpoint:       cfg.S3Endpoint,
			Region:         cfg.S3Region,
			Bucket:         cfg.S3Bucket,
			AccessKey:      cfg.S3AccessKey,
			SecretKey:      cfg.S3SecretKey,
			SessionToken:   cfg.S3SessionToken,
			ForcePathStyle: cfg.S3ForcePathStyle,
		})
	default:
		return nil, fmt.Errorf("unsupported storage driver %q", cfg.StorageDriver)
	}
}
