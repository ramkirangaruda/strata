package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// webFS embeds the whole frontend - four pages plus their shared static
// assets - so the server binary stays fully self-contained: no separate
// static-file deploy step, and it works identically whether this runs via
// `go run`, in the Docker image, or as a Vercel Function.
//
//go:embed web
var webFS embed.FS

// handlePage serves one embedded HTML page from web/.
func (s *server) handlePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webFS, "web/"+name)
	}
}

// staticHandler serves web/static/* under /static/ - the CSS and JS shared
// by every page.
func staticHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web/static")
	if err != nil {
		// Only reachable if the embed directive above stops matching a
		// web/static directory that exists at build time - a build-time
		// bug, not something a request can trigger.
		panic(err)
	}
	return http.StripPrefix("/static/", http.FileServerFS(sub))
}
