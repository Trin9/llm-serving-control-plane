#!/usr/bin/env bash
# archive-evidence.sh — bundle + checksum git-ignored evidence directories.
#
# Creates versioned, checksummed tarballs of the local experiment evidence so
# the git-ignored artifacts survive independently of the repo and the GPU
# quota deadline. A manifest records source path, size, and SHA256 for each
# bundle so the archive can be round-trip verified later.
#
# Usage:
#   scripts/archive-evidence.sh [dest_dir] [artifact_dir ...]
#
# Defaults:
#   dest_dir     = ~/llm-evidence-archive
#   artifact_dir = artifacts/phase5-azure artifacts/phase6-azure
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${1:-$HOME/llm-evidence-archive}"
shift 2>/dev/null || true
if [ "$#" -eq 0 ]; then
  set -- "artifacts/phase5-azure" "artifacts/phase6-azure"
fi

TS="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$DEST"
cd "$REPO_ROOT"

MANIFEST="$DEST/manifest-$TS.json"
SUMS="$DEST/SHA256SUMS-$TS.txt"
: > "$SUMS"

echo "{" > "$MANIFEST"
echo "  \"created_utc\": \"$TS\"," >> "$MANIFEST"
echo "  \"bundles\": [" >> "$MANIFEST"

first=1
for dir in "$@"; do
  if [ ! -d "$dir" ]; then
    echo "SKIP: not a directory: $dir" >&2
    continue
  fi
  name="$(basename "$dir")"
  tarball="$DEST/${name}-${TS}.tar.gz"
  echo "bundling $dir -> $tarball"
  tar -czf "$tarball" -C "$REPO_ROOT" "$dir"

  sha="$(sha256sum "$tarball" | awk '{print $1}')"
  size="$(stat -c %s "$tarball")"
  echo "$sha  ${name}-${TS}.tar.gz" >> "$SUMS"

  if [ "$first" -eq 1 ]; then first=0; else echo "," >> "$MANIFEST"; fi
  printf '    {"source": "%s", "bundle": "%s-%s.tar.gz", "bytes": %s, "sha256": "%s"}' \
    "$dir" "$name" "$TS" "$size" "$sha" >> "$MANIFEST"
done

echo "" >> "$MANIFEST"
echo "  ]" >> "$MANIFEST"
echo "}" >> "$MANIFEST"

echo "---- archive complete ----"
echo "dest:    $DEST"
echo "manifest: $MANIFEST"
echo "sums:     $SUMS"
ls -la "$DEST"
echo "---- verify checksums ----"
(cd "$DEST" && sha256sum -c "$(basename "$SUMS")")
