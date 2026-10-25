// Package migrations embeds the schema so the binary can migrate itself.
package migrations

import "embed"

//go:embed *.up.sql
var Files embed.FS
