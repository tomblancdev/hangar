// Package ui carries the mark, and the console: the app a browser loads,
// drawn from what the brain serves — static files, embedded as they are (no
// build step: what is in this directory is what runs).
package ui

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

//go:embed console
var consoleFS embed.FS

// Console is the console's app: index.html, and beside it what the page
// loads (served under static/).
func Console() fs.FS {
	sub, err := fs.Sub(consoleFS, "console")
	if err != nil {
		panic(err) // the directory is embedded above
	}
	return sub
}
