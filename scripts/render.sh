#!/usr/bin/env bash
# Render the manifests for one Teleport cluster.
#
#   PROXY_ADDR=example.teleport.sh:443 scripts/render.sh | kubectl apply -f -
#
# Variables:
#   PROXY_ADDR        Teleport Proxy Service address, host:port (required)
#   TELEPORT_CLUSTER  Teleport cluster name = the projected token's audience
#                     (default: PROXY_ADDR without the port)
#   BOT_NAME          bot name; also the token name and the database user prefix (default: db-status)
#   TOKEN_NAME        join token name (default: BOT_NAME)
#   TBOT_VERSION      tbot image tag (default: the cluster's server_version, read from /webapi/ping)
#   IMAGE             application image (default: ghcr.io/jsabo/teleport-machine-identity-k8s:latest)
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
: "${PROXY_ADDR:?set PROXY_ADDR (host:port)}"
TELEPORT_CLUSTER="${TELEPORT_CLUSTER:-${PROXY_ADDR%%:*}}"
BOT_NAME="${BOT_NAME:-db-status}"
TOKEN_NAME="${TOKEN_NAME:-$BOT_NAME}"
IMAGE="${IMAGE:-ghcr.io/jsabo/teleport-machine-identity-k8s:latest}"
if [ -z "${TBOT_VERSION:-}" ]; then
  TBOT_VERSION=$(curl -fsS --max-time 5 "https://${PROXY_ADDR}/webapi/ping" | sed -n 's/.*"server_version":"\([^"]*\)".*/\1/p')
  [ -n "$TBOT_VERSION" ] || { echo "render.sh: could not read server_version from ${PROXY_ADDR}; set TBOT_VERSION" >&2; exit 1; }
fi

for f in namespace serviceaccount configmap deployment service; do
  sed -e "s|\${PROXY_ADDR}|${PROXY_ADDR}|g" \
      -e "s|\${TOKEN_NAME}|${TOKEN_NAME}|g" \
      -e "s|\${TELEPORT_CLUSTER}|${TELEPORT_CLUSTER}|g" \
      -e "s|\${BOT_NAME}|${BOT_NAME}|g" \
      -e "s|\${TBOT_VERSION}|${TBOT_VERSION}|g" \
      -e "s|\${IMAGE}|${IMAGE}|g" \
      "${here}/k8s/${f}.yaml"
  echo '---'
done
