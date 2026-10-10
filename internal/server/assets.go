package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFS holds the design system shared by every window (main list, New Download,
// status, completion): theme.css (colour tokens for all themes + primitives) and
// icons.js (the SVG icon library, window.DBI). Pages link them from /ui/….
//
//go:embed web/ui
var uiFS embed.FS

// uiAssets serves web/ui/* at /ui/*. no-cache (revalidate) so a rebuilt theme or
// icon set shows immediately instead of coming stale out of WebView2's disk cache.
func uiAssets() http.Handler {
	sub, err := fs.Sub(uiFS, "web/ui")
	if err != nil {
		panic(err) // the embed path is fixed at compile time
	}
	files := http.StripPrefix("/ui/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
