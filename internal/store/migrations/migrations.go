// Package migrations embeds the goose migrations for the durable schemas raw, sync and secret.
// They are additive only: there are no Down sections, and a released file is never edited.
package migrations

import "embed"

// FS holds the migration files at its root.
//
//go:embed *.sql
var FS embed.FS
