// Package migrations embeds the audit schema migrations.
package migrations

import "embed"

// FS holds the *.sql files of this directory.
//
//go:embed *.sql
var FS embed.FS
