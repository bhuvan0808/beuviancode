// Package migrations embeds Beuvian's versioned SQL migrations into the binary.
//
// This package exists solely to host the //go:embed directive. go:embed cannot
// reference a parent directory, so embedding from internal/adapter/postgres would
// have forced the SQL files to live under that package — burying the schema
// somewhere nobody looks. Keeping them at backend/migrations/ (where DATABASE.md
// and the README point) and exporting an fs.FS costs one small file and keeps the
// schema where it belongs.
//
// Embedding rather than reading from disk means the container needs no migration
// files copied alongside it, and a binary can never be run against a mismatched
// set of migrations: the schema it expects travels with it.
package migrations

import "embed"

// FS holds every .sql migration.
//
// Filenames must be NNNN_description.up.sql / NNNN_description.down.sql. The
// migrator rejects anything else rather than skipping it, because a silently
// ignored migration file is how a schema change goes missing in production.
//
//go:embed *.sql
var FS embed.FS
