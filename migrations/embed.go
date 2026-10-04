// Package migrations는 DB migration SQL을 바이너리에 포함한다 (ADR 0016).
package migrations

import "embed"

// Postgres는 제어 DB migration이다. 파일명 규칙: NNNNN_name.sql (goose 형식).
//
//go:embed postgres/*.sql
var Postgres embed.FS
