# Changelog

Notable changes to the repo, as opposed to the engine's own phase plan
(`docs/DESIGN.md` §7 tracks that separately).

## Unreleased

- Added `cmd/server`, a small HTTP KV-store demo built on the engine
  (`GET/PUT/DELETE /kv/{key}`, `/healthz`, a JSON index route), with
  structured request logging, graceful shutdown, and env-based config
  (`PORT`, `STRATA_DATA_DIR`, `STRATA_SYNC`).
- Added Docker support (`Dockerfile`, `docker-compose.yml`, `.dockerignore`)
  with a persistent volume for the demo server.
- Added zero-config Vercel deployment (`vercel.json`) for the demo server,
  with the ephemeral-storage tradeoff documented in `docs/DEPLOYMENT.md`.
- Added GitHub Actions CI: build/vet/test, a separate race-detector job,
  golangci-lint, and a Docker build check.
- Added `examples/basic`, a minimal library-usage example.
- Added `scripts/smoke.sh` to verify a live deployment end to end.
- Added an architecture-at-a-glance diagram to the README.
- Added `LICENSE` (MIT), `CONTRIBUTING.md`, `.env.example`.
- Fixed `removeObsoleteFiles` to use `filepath.Join` instead of manual
  path concatenation.

## Phase A1

- Write path, durability, and crash recovery. See `docs/DESIGN.md` and the
  README's reading order for the full writeup.
