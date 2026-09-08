// Command server is a small HTTP KV-store demo built on top of the strata
// engine. It exists to give the library something you can deploy and poke at
// with curl - it is deliberately not a showcase of strata's own feature set,
// since phase A1 is a point-lookup engine and that is exactly what this
// exposes: get, put, delete, nothing more.
package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ramkirangaruda/strata"
)

// server wires an *strata.DB to a set of HTTP handlers.
type server struct {
	db  *strata.DB
	log *slog.Logger
}

func newServer(db *strata.DB, log *slog.Logger) *server {
	return &server{db: db, log: log}
}

// routes builds the mux. Using Go 1.22+ method+pattern routing keeps this to
// stdlib - there is exactly one dependency in this whole repo (none), and a
// KV demo is not worth breaking that streak for a router library.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /kv/{key}", s.handleGet)
	mux.HandleFunc("PUT /kv/{key}", s.handlePut)
	mux.HandleFunc("DELETE /kv/{key}", s.handleDelete)
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /api", s.handleAPIInfo)
	return s.withLogging(mux)
}

// withLogging logs one line per request: method, path, status, and latency.
// Nothing fancier than that is needed for a demo server with three routes.
func (s *server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusRecorder captures the status code a handler wrote, since
// http.ResponseWriter does not expose it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
