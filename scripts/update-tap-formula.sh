#!/usr/bin/env bash
# Regenerates Formula/kubectl-mole.rb in justin-tahara/homebrew-tap from a
# release's checksum file and pushes it.
#
# Usage: update-tap-formula.sh <tag> [checksums-file]
# Needs: HOMEBREW_TAP_TOKEN with contents write access to the tap repo.
set -euo pipefail

tag="$1"
checksums="${2:-dist/checksums.txt}"
version="${tag#v}"
tmpl="$(dirname "$0")/tap-formula.tmpl.rb"

sha() {
  awk -v f="kubectl-mole_${tag}_$1.tar.gz" '$2 == f {print $1}' "$checksums"
}

darwin_arm64=$(sha darwin_arm64)
darwin_amd64=$(sha darwin_amd64)
linux_arm64=$(sha linux_arm64)
linux_amd64=$(sha linux_amd64)
for v in "$darwin_arm64" "$darwin_amd64" "$linux_arm64" "$linux_amd64"; do
  if [ -z "$v" ]; then
    echo "missing checksum in $checksums" >&2
    exit 1
  fi
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git clone --quiet --depth 1 \
  "https://x-access-token:${HOMEBREW_TAP_TOKEN}@github.com/justin-tahara/homebrew-tap.git" "$tmp/tap"

sed -e "s/__VERSION__/${version}/" \
    -e "s/__SHA_DARWIN_ARM64__/${darwin_arm64}/" \
    -e "s/__SHA_DARWIN_AMD64__/${darwin_amd64}/" \
    -e "s/__SHA_LINUX_ARM64__/${linux_arm64}/" \
    -e "s/__SHA_LINUX_AMD64__/${linux_amd64}/" \
    "$tmpl" > "$tmp/tap/Formula/kubectl-mole.rb"

cd "$tmp/tap"
if git diff --quiet; then
  echo "formula already up to date"
  exit 0
fi
git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git commit --quiet -am "kubectl-mole ${version}"
git push --quiet origin HEAD:main
echo "tap formula updated to ${version}"
