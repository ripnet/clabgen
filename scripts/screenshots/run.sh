#!/usr/bin/env bash
# The README tour (docs/images/tour.webp) and the docs' stills, each in a
# light and a dark version: deploys the demo lab, runs a UI that sees only
# it, drives the UI through the tour (tour.mjs), builds the animations
# (animate.py) and tears the lab down. Needs containerlab, Docker, Node
# with web/'s packages (npm ci in web/), Playwright's Chromium (or
# PLAYWRIGHT_CHROMIUM) and Python with Pillow. CLABGEN_IMAGE and
# CLABGEN_UI_IMAGE pick the images (default: the latest release).
set -euo pipefail
cd "$(dirname "$0")"
images=../../docs/images
ui_image=${CLABGEN_UI_IMAGE:-ghcr.io/ripnet/clabgen-ui:latest}
port=${SHOTS_PORT:-8099}
work=$(mktemp -d)

cleanup() {
  docker rm -f clabgen-ui-shots >/dev/null 2>&1 || true
  clab destroy -t demo.clab.yml --cleanup >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

docker run -d --rm --name clabgen-ui-shots --network host -v "$PWD:$PWD:ro" "$ui_image" \
  -listen "127.0.0.1:$port" -docker off -lab "$PWD/demo.clab.yml" -update-check=false >/dev/null

for scheme in light dark; do
  suffix=$([ "$scheme" = dark ] && echo -dark || true)
  # A fresh lab each time: no history from the last run's failure.
  clab deploy -t demo.clab.yml --reconfigure >/dev/null
  for _ in $(seq 60); do
    [ "$(curl -s "http://127.0.0.1:$port/api/agents" | grep -o clab-demo- | wc -l)" -ge 2 ] && break
    sleep 1
  done
  node tour.mjs "http://127.0.0.1:$port/" "$work/$scheme" "$scheme"
  python3 animate.py "$work/$scheme" "$images/tour$suffix.webp"
  for still in bgp protocol group-editor capture flow form-device form-mep form-bgp form-ping form-packet form-group; do
    cp "$work/$scheme/$still.png" "$images/$still$suffix.png"
  done
done
