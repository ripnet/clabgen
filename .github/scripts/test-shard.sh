#!/usr/bin/env bash
# test-shard.sh SHARD COUNT [go test flags...]: runs every COUNT-th test of
# internal/core, starting at SHARD (0-based), in `go test -list` order.
# Neighbouring tests in a file tend to be alike (the OSPF ones all wait on
# protocol timers), so dealing them out round-robin keeps shards even, and
# new tests join a shard without any bookkeeping.
set -euo pipefail
shard=$1 count=$2
shift 2
pkg=./internal/core

mapfile -t tests < <(go test -list . "$pkg" | grep '^Test')
mine=()
for i in "${!tests[@]}"; do
  if (( i % count == shard )); then mine+=("${tests[$i]}"); fi
done
if (( ${#mine[@]} == 0 )); then
  echo "shard $shard/$count: no tests"
  exit 0
fi
echo "shard $shard/$count: ${#mine[@]} of ${#tests[@]} tests: ${mine[*]}"
re="^($(IFS='|'; echo "${mine[*]}"))\$"
exec go test -count=1 "$@" -run "$re" "$pkg"
