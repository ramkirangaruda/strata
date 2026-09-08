# Deploying the demo server

`cmd/server` is a small HTTP wrapper around the engine: `GET/PUT/DELETE
/kv/{key}`, plus `/healthz`. It exists so the repo has something you can
actually hit with `curl` rather than just read. It is not a demonstration of
strata's own scalability - phase A1 is a point-lookup engine with everything
in level 0, and this server does nothing to change that.

## Vercel

The repo is set up for zero-config deployment: `vercel.json` sets
`"framework": "go"`, and Vercel's Go preset auto-detects `cmd/server/main.go`
as the entrypoint and builds it with the `go` version declared in `go.mod`.

1. Push this repo to GitHub (already done if you're reading this from the
   repo).
2. On [vercel.com](https://vercel.com), **Add New -> Project**, import the
   `strata` repo, and deploy. No build command or environment variables are
   required.
3. Every push to `main` redeploys automatically.

**Read this before you draw conclusions from it.** Vercel Functions - like
every mainstream serverless platform - give a function a writable `/tmp` and
nothing else: no attached disk, no guarantee two requests land on the same
instance, and no guarantee an instance survives past a short idle window.
`cmd/server` detects this (`VERCEL=1` is set automatically) and points
strata at `/tmp/strata-data` so it *runs*, but the data you `PUT` can
disappear on the very next request if it lands on a different instance, and
*will* disappear on redeploy. That is a property of Vercel, not a bug in
strata - the engine's durability guarantees are about surviving `kill -9` on
a real disk (see the crash-recovery test in `db_test.go`), and Vercel never
gives it one. Treat a Vercel deployment as a live API demo, not a durability
demo.

## Docker (with real persistence)

If you want to see strata actually keep data across restarts, run it
somewhere with a real volume:

```bash
docker compose up --build
curl -X PUT --data 'hello' localhost:8080/kv/greeting
docker compose restart
curl localhost:8080/kv/greeting   # still "hello" - recovery ran on the restart
```

`docker-compose.yml` mounts a named volume at `/data` and sets
`STRATA_SYNC=true`, so every acknowledged write is fsynced before the
request returns.

The same image deploys to any platform that gives you a persistent volume
and lets you run a container - Fly.io and Render both have free tiers that
do, if you want a public URL with real durability behind it rather than
Vercel's ephemeral `/tmp`.

## Configuration

All of it is environment variables, read in `cmd/server/config.go`:

| Variable          | Default    | Meaning                                          |
|--------------------|------------|---------------------------------------------------|
| `PORT`             | `8080`     | HTTP listen port                                   |
| `STRATA_DATA_DIR`  | `./data`   | Where the database lives (`/tmp/strata-data` on Vercel automatically) |
| `STRATA_SYNC`      | `false`    | fsync every write; see `docs/DESIGN.md` §4 for what this actually promises |
