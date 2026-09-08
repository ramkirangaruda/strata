package main

import (
	"embed"
	"net/http"
)

// webFS embeds the frontend so the server binary is fully self-contained -
// no separate static-file deploy step, and it works identically whether
// this runs via `go run`, in the Docker image, or as a Vercel Function.
//
//go:embed web/index.html
var webFS embed.FS

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, webFS, "web/index.html")
}
