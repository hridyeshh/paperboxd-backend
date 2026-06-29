package migrations

import "embed"

// Files holds the SQL migration files embedded into the binary so the app can
// run migrations on startup without the migrate CLI or the source tree present.
//
//go:embed *.sql
var Files embed.FS
