// Package migrations embeds the SQL schema migrations that are applied
// by internal/database. Migration files are named NNNN_description.sql
// and are applied in ascending numeric order, each inside its own
// transaction. Migrations are up-only: schema changes must be additive
// or handled by a later migration, never by editing an applied file.
package migrations

import "embed"

// FS contains all SQL migration files.
//
//go:embed *.sql
var FS embed.FS
