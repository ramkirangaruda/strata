package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ramkirangaruda/strata"
)

// maxValueSize bounds how much of a request body handlePut will read. A demo
// server with no auth in front of it should not let one request allocate an
// unbounded amount of memory.
const maxValueSize = 1 << 20 // 1 MiB

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":  "strata",
		"phase": "A1 of 6 - write path, durability, recovery",
		"repo":  "https://github.com/ramkirangaruda/strata",
		"routes": []string{
			"GET    /healthz",
			"GET    /kv/{key}",
			"PUT    /kv/{key}  (body is the value, stored as raw bytes)",
			"DELETE /kv/{key}",
		},
	})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	value, err := s.db.Get([]byte(key))
	if errors.Is(err, strata.ErrNotFound) {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		s.log.Error("get failed", "key", key, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(value)
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxValueSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(body) > maxValueSize {
		writeError(w, http.StatusRequestEntityTooLarge, "value exceeds 1 MiB")
		return
	}
	if err := s.db.Put([]byte(key), body); err != nil {
		s.log.Error("put failed", "key", key, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.db.Delete([]byte(key)); err != nil {
		s.log.Error("delete failed", "key", key, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
