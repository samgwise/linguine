#!/usr/bin/env bash
# Fetch the newest green build of main from the rolling "latest" GitHub
# release and stage it locally.
#
# Usage: ./pull-latest.sh [amd64|arm64] [target-dir]
#
# This script deliberately does NOT restart or manage any services — picking
# up a new build (e.g. systemctl restart linguine) is the server's own
# configuration/stack responsibility.
set -euo pipefail

REPO="samgwise/linguine"
ARCH="${1:-amd64}"
TARGET="${2:-.}"
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "error: unsupported arch '$ARCH' (use amd64 or arm64)" >&2; exit 2 ;;
esac

BASE="https://github.com/${REPO}/releases/latest/download"
TARBALL="linguine-linux-${ARCH}.tar.gz"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# The publish job briefly deletes the "latest" release before recreating it,
# so a 404 can be transient — retry a few times before giving up.
fetch() {
  local url="$1" out="$2" attempt
  for attempt in 1 2 3 4 5; do
    if curl -fsSL --retry 2 -o "$out" "$url"; then
      return 0
    fi
    echo "fetch failed (attempt $attempt/5), retrying in 10s..." >&2
    sleep 10
  done
  echo "error: could not download $url" >&2
  return 1
}

echo "Fetching ${TARBALL} from ${REPO} (rolling latest)..."
fetch "${BASE}/${TARBALL}" "${STAGE}/${TARBALL}"
fetch "${BASE}/${TARBALL}.sha256" "${STAGE}/${TARBALL}.sha256"

echo "Verifying checksum..."
(cd "$STAGE" && sha256sum -c "${TARBALL}.sha256")

echo "Extracting to ${TARGET}..."
tar -xzf "${STAGE}/${TARBALL}" -C "$TARGET"

echo
echo "Staged binaries in ${TARGET}:"
( cd "$TARGET" && echo "  linguine $(./linguine version 2>/dev/null || echo '(version check failed)')" )
echo
echo "Done. Restart the service (systemd unit / deploy stack) to pick up the new build."
