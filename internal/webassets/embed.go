package webassets

import "embed"

// FS contains the server's browser UI. Embedding keeps the production binary self-contained.
//
//go:embed web/*
var FS embed.FS
