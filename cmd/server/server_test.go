package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ramkirangaruda/strata"
)

func testServer(t *testing.T) *server {
	t.Helper()
	db, err := strata.Open(t.TempDir(), strata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newServer(db, log)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	h := testServer(t).routes()
	rec := do(t, h, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestKVRoundTrip(t *testing.T) {
	h := testServer(t).routes()

	if rec := do(t, h, http.MethodGet, "/kv/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing key: status = %d, want 404", rec.Code)
	}

	if rec := do(t, h, http.MethodPut, "/kv/greeting", "hello, strata"); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}

	rec := do(t, h, http.MethodGet, "/kv/greeting", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "hello, strata" {
		t.Fatalf("GET body = %q, want %q", got, "hello, strata")
	}

	if rec := do(t, h, http.MethodDelete, "/kv/greeting", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status = %d, want 204", rec.Code)
	}

	if rec := do(t, h, http.MethodGet, "/kv/greeting", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: status = %d, want 404 (tombstone must shadow the old value)", rec.Code)
	}
}

func TestPutOverwrite(t *testing.T) {
	h := testServer(t).routes()
	do(t, h, http.MethodPut, "/kv/k", "v1")
	do(t, h, http.MethodPut, "/kv/k", "v2")
	rec := do(t, h, http.MethodGet, "/kv/k", "")
	if got := rec.Body.String(); got != "v2" {
		t.Fatalf("GET after overwrite = %q, want %q", got, "v2")
	}
}

func TestIndex(t *testing.T) {
	h := testServer(t).routes()
	rec := do(t, h, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestPutTooLargeIsRejected(t *testing.T) {
	h := testServer(t).routes()
	oversized := strings.Repeat("x", maxValueSize+1)
	rec := do(t, h, http.MethodPut, "/kv/big", oversized)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	// The rejected write must not have landed - nothing to overwrite the
	// missing key with.
	if rec := do(t, h, http.MethodGet, "/kv/big", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a rejected PUT should not have written anything: status = %d, want 404", rec.Code)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	h := testServer(t).routes()
	rec := do(t, h, http.MethodGet, "/not-a-route", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
