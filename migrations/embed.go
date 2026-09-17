// Package migrations embeds the SQL migration files so the aggregator
// binary carries its own schema history and needs no filesystem access to
// run "migrate-up" / "migrate-down".
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
