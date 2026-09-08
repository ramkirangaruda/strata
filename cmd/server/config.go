package main

import (
	"os"
	"strconv"
)

// config holds everything main needs to start the server, gathered from the
// environment so the same binary runs unmodified locally, in Docker, and on
// a PaaS that injects its own PORT.
type config struct {
	// addr is the "host:port" (or ":port") the HTTP server listens on.
	addr string

	// dataDir is where strata keeps its WAL, SSTables and manifest.
	//
	// Vercel's Go runtime, and most serverless platforms, give a function a
	// writable /tmp and nothing else - no attached volume, and no promise
	// that two requests land on the same instance. strata's durability
	// guarantees assume a real, persistent disk, so on a platform like that
	// this server still runs and answers requests, it just is not the
	// durability demo the rest of this repo is about. See
	// docs/DEPLOYMENT.md for the honest version of that tradeoff.
	dataDir string

	// sync mirrors strata.Options.Sync. Off by default here because the
	// point of this server is to show the API, not to benchmark fsync
	// latency; set STRATA_SYNC=1 to turn it on.
	sync bool
}

func configFromEnv() config {
	c := config{
		addr:    ":8080",
		dataDir: "./data",
		sync:    false,
	}
	if p := os.Getenv("PORT"); p != "" {
		c.addr = ":" + p
	}
	if d := os.Getenv("STRATA_DATA_DIR"); d != "" {
		c.dataDir = d
	} else if os.Getenv("VERCEL") != "" {
		// Vercel functions can only write under /tmp. Detect it automatically
		// so the same binary works there with zero configuration, rather than
		// failing on an unwritable ./data.
		c.dataDir = "/tmp/strata-data"
	}
	if s := os.Getenv("STRATA_SYNC"); s != "" {
		if b, err := strconv.ParseBool(s); err == nil {
			c.sync = b
		}
	}
	return c
}
