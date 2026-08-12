#!/usr/bin/env bash
# Installs the pinned goreleaser with retried, checksum-verified downloads
# and runs it with the given arguments. Shared by ci.yaml (check) and
# release.yaml (release) so the pin cannot drift between them — the
# action's raw binary download has no retry and flakes on runner network
# resets (it cost the v0.2.0 release a rerun). Linux amd64 only: this is
# a CI-runner helper; locally, `brew install goreleaser`.
set -euo pipefail

GORELEASER_VERSION="${GORELEASER_VERSION:-v2.17.1}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
base="https://github.com/goreleaser/goreleaser/releases/download/${GORELEASER_VERSION}"
curl -fsSL --retry 5 --retry-all-errors --output-dir "$tmp" -O "$base/goreleaser_Linux_x86_64.tar.gz"
curl -fsSL --retry 5 --retry-all-errors --output-dir "$tmp" -O "$base/checksums.txt"
(cd "$tmp" && sha256sum --check --ignore-missing checksums.txt >/dev/null)
tar -xzf "$tmp/goreleaser_Linux_x86_64.tar.gz" -C "$tmp" goreleaser
exec "$tmp/goreleaser" "$@"
