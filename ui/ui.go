// Package ui carries the mark. The console that will live here is drawn from
// the plugins' schemas; until it exists, the front page is the mark and a
// pointer to the API.
package ui

import "embed"

//go:embed static
var staticFS embed.FS
