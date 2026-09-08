# Changelog

Notable changes to the repo, as opposed to the engine's own phase plan
(`docs/DESIGN.md` §7 tracks that separately).

## Unreleased

- Added `cmd/server`, a small HTTP KV-store demo built on the engine
  (`GET/PUT/DELETE /kv/{key}`, `/healthz`).
- Added Docker support (`Dockerfile`, `docker-compose.yml`) with a
  persistent volume for the demo server.
- Added zero-config Vercel deployment (`vercel.json`) for the demo server,
  with the ephemeral-storage tradeoff documented in `docs/DEPLOYMENT.md`.
- Added GitHub Actions CI: build, vet, test, and a separate race-detector
  job.
- Added `examples/basic`, a minimal library-usage example.
- Added `LICENSE` (MIT), `CONTRIBUTING.md`.
- Fixed `removeObsoleteFiles` to use `filepath.Join` instead of manual
  path concatenation.

## Phase A1

- Write path, durability, and crash recovery. See `docs/DESIGN.md` and the
  README's reading order for the full writeup.
