#!/usr/bin/env bash
# An example's screenshots for docs/examples, in a light and a dark
# version: runs a UI that sees only the example's lab and drives it with
# example-NAME.mjs, saving to docs/images/examples (and, when the script
# records frames, NAME-actions.webp from them, by animate.py). The lab must
# be deployed and settled first (most need SR-SIM and its license):
#   scripts/screenshots/example.sh dhcp
# Needs Docker, Node with web/'s packages (npm ci in web/), Playwright's
# Chromium (or PLAYWRIGHT_CHROMIUM) and Python with Pillow.
# CLABGEN_UI_IMAGE picks the UI image (default: the latest release).
set -euo pipefail
cd "$(dirname "$0")"
name=$1
lab=$(realpath "../../examples/$name/$name.clab.yml")
images=../../docs/images/examples
ui_image=${CLABGEN_UI_IMAGE:-ghcr.io/ripnet/clabgen-ui:latest}
port=${SHOTS_PORT:-8099}
work=$(mktemp -d)

cleanup() {
  docker rm -f clabgen-ui-shots >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

docker run -d --rm --name clabgen-ui-shots --network host -v "$(dirname "$lab"):$(dirname "$lab"):ro" "$ui_image" \
  -listen "127.0.0.1:$port" -docker off -lab "$lab" -update-check=false >/dev/null
for _ in $(seq 30); do
  curl -sf "http://127.0.0.1:$port/api/agents" | grep -q "clab-$name-" && break
  sleep 1
done
mkdir -p "$images"
for scheme in light dark; do
  suffix=$([ "$scheme" = dark ] && echo -dark || true)
  node "example-$name.mjs" "http://127.0.0.1:$port/" "$images" "$work/$scheme" "$scheme"
  if [ -f "$work/$scheme/frames.json" ]; then
    python3 animate.py "$work/$scheme" "$images/$name-actions$suffix.webp"
  fi
done
if [ -f "diagram-$name.py" ]; then
  python3 "diagram-$name.py" "$images"
fi
