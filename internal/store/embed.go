package store

import _ "embed"

// schemaSQL is the contents of schema.sql, applied idempotently by Open.
//
//go:embed schema.sql
var schemaSQL string
