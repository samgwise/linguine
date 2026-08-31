#!/usr/bin/env bash
# Auto-deploy new builds of linguine: poll the rolling "latest" release's
# checksum asset and run deploy.sh whenever it changes. Intended to be driven
# by a systemd user timer (see the README's "Automatic self-deployment"
# section); safe to run by hand as well.
#
# The .sha256 asset is tiny (~100 bytes), so polling is negligible traffic.
# The checksum guards the download's integrity; release authenticity would
# need signed releases, which is a separate piece of work.
set -euo pipefail

BIN_DIR="${LINGUINE_BIN_DIR:-$HOME/linguine/bin}"
STAMP="${LINGUINE_STAMP:-$HOME/linguine/.deployed-sha256}"
ARCH="${LINGUINE_ARCH:-amd64}"
URL="https://github.com/samgwise/linguine/releases/latest/download/linguine-linux-${ARCH}.tar.gz.sha256"
LOCK="/tmp/linguine-autoupdate.lock"
DEPLOY="${BIN_DIR}/deploy.sh"

if [ ! -x "$DEPLOY" ] && [ ! -f "$DEPLOY" ]; then
  echo "error: deploy script not found at $DEPLOY" >&2
  echo "(deploy.sh pairs with this script: pull, swap binaries, restart, roll back on failure)" >&2
  exit 1
fi

# One update at a time; a queued timer run just skips if another holds the lock.
exec 9>"$LOCK"
if ! flock -n 9; then
  echo "another autoupdate run is in progress; skipping"
  exit 0
fi

# Retry a few times: the publish job briefly recreates the "latest" release,
# so a 404 right after a push is expected and transient.
want=""
for attempt in 1 2 3; do
  if want="$(curl -fsSL --retry 2 --max-time 30 "$URL")"; then
    break
  fi
  echo "fetch failed (attempt $attempt/3), retrying in 10s..." >&2
  sleep 10
done
if [ -z "$want" ]; then
  echo "error: could not fetch latest checksum" >&2
  exit 1
fi

have="$(cat "$STAMP" 2>/dev/null || true)"
if [ "$want" = "$have" ]; then
  echo "up to date ($(printf '%s' "$want" | cut -d' ' -f1 | cut -c1-12))"
  exit 0
fi

echo "new build detected (${have:-no stamp} -> $want); deploying..."
if bash "$DEPLOY"; then
  # Restamp only on success: a failed or rolled-back deploy leaves the old
  # stamp, so the next cycle retries the same build.
  printf '%s\n' "$want" > "$STAMP"
  echo "autoupdate: deployed new build"
else
  echo "error: deploy failed; will retry next cycle" >&2
  exit 1
fi
