// Package migrations embeds the notifications schema migrations.
package migrations

import "embed"

// FS holds the *.sql files of this directory.
//
//go:embed *.sql
var FS embed.FS
