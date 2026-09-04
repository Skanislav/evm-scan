// Package migrations embeds the SQL schema so a binary can bootstrap its own
// database without the .sql files being deployed alongside it.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
