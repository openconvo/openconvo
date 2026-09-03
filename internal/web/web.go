// Package web embeds the compiled frontend assets. The dist directory
// is populated by the Vite build (make web); in git it contains only a
// placeholder so that the Go toolchain works without Node.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the built frontend as a filesystem rooted at dist,
// and whether a real build (index.html) is present.
func Assets() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return sub, false
	}
	return sub, true
}
