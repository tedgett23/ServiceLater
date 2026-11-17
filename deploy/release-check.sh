#!/usr/bin/env bash
# The check before a release (make release-check): a fresh all-in-one Compose stack from the image (SL_IMAGE,
# servicelater:latest by default; `make image` builds it), with throwaway secrets, then deploy/smoke.sh against it,
# then everything removed again, volumes included. COMPOSE picks the tool: "docker compose" (default) or podman-compose.
# Every variable the Compose file reads is written out: podman-compose leaves ${VAR:-default} as it is when VAR is unset.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
compose=${COMPOSE:-docker compose}
port=8080 # the Compose file publishes 8080
work=$(mktemp -d)
env="$work/release-check.env"
cat > "$env" <<ENV
POSTGRES_PASSWORD=$(openssl rand -hex 16)
SL_JWT_SECRET=$(openssl rand -base64 48 | tr -d '\n')
SL_SECRETS_KEY=$(openssl rand -base64 32 | tr -d '\n')
SL_BOOTSTRAP_ADMIN_EMAIL=release-check@example.com
SL_BOOTSTRAP_ADMIN_PASSWORD=release-check-password
SL_IMAGE=${SL_IMAGE:-servicelater:latest}
SL_TLS=off
SL_DOMAIN=
ENV
stack() { $compose -p slreleasecheck -f "$here/compose/docker-compose.yml" --env-file "$env" "$@"; }
cleanup() { stack down -v >/dev/null 2>&1 || true; rm -r "$work"; }
trap cleanup EXIT

stack up -d
for _ in $(seq 1 90); do curl -fsS "http://localhost:$port/healthz" >/dev/null 2>&1 && break; sleep 1; done
if ! "$here/smoke.sh" "http://localhost:$port" release-check@example.com release-check-password; then
	echo "--- the app's log:"
	stack logs app 2>&1 | tail -40
	exit 1
fi
