package migrations

import "embed"

// Files contains the ordered SQL migrations applied by shift-control.
//
//go:embed migrations/*.sql
var Files embed.FS
