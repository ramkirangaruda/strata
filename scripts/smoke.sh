#!/usr/bin/env bash
# Smoke-tests a running cmd/server: put a key, read it back, delete it, and
# confirm the delete stuck. Run it against anything - localhost, a Docker
# container, or a fresh Vercel deploy:
#
#   ./scripts/smoke.sh http://localhost:8080
#   ./scripts/smoke.sh https://strata-yourname.vercel.app
set -euo pipefail

base="${1:-http://localhost:8080}"
key="smoke-$(date +%s)"

echo "==> health"
curl -fsS "$base/healthz"; echo

echo "==> put $key"
curl -fsS -X PUT --data 'hello from smoke.sh' "$base/kv/$key"; echo

echo "==> get $key"
got="$(curl -fsS "$base/kv/$key")"
[ "$got" = "hello from smoke.sh" ] || { echo "FAIL: got '$got'"; exit 1; }
echo "$got"

echo "==> delete $key"
curl -fsS -o /dev/null -w '%{http_code}\n' -X DELETE "$base/kv/$key"

echo "==> get after delete (expect 404)"
code="$(curl -s -o /dev/null -w '%{http_code}' "$base/kv/$key")"
[ "$code" = "404" ] || { echo "FAIL: expected 404, got $code"; exit 1; }
echo "$code"

echo "OK"
