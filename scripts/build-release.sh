#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

version="${VERSION:-}"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  printf 'VERSION must be a release tag, for example v0.1.0 or v0.1.0-rc.1\n' >&2
  exit 1
fi

go_binary="${GO:-go}"
commit="${COMMIT:-$(git rev-parse HEAD)}"
source_epoch="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}"
if [[ ! "$commit" =~ ^[0-9a-f]{40}$ && "$commit" != unknown ]]; then
  printf 'COMMIT must be a full Git commit SHA or unknown\n' >&2
  exit 1
fi
if [[ ! "$source_epoch" =~ ^[0-9]+$ ]]; then
  printf 'SOURCE_DATE_EPOCH must be a non-negative integer\n' >&2
  exit 1
fi
build_date="$(date -u -d "@$source_epoch" +%Y-%m-%dT%H:%M:%SZ)"
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$build_date"

export LC_ALL=C
mkdir -p dist
staging="$(mktemp -d)"
trap 'rm -rf -- "$staging"' EXIT
cp README.md LICENSE "$staging/"
cp -R examples docs "$staging/"
printf 'version=%s\ncommit=%s\ndate=%s\ntoolchain=%s\n' \
  "$version" "$commit" "$build_date" "$("$go_binary" version)" > "$staging/BUILDINFO"

for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_binary" build \
    -trimpath -buildvcs=false -ldflags "$ldflags" \
    -o "$staging/garm-provider-timeweb" ./cmd/garm-provider-timeweb
  archive="dist/garm-provider-timeweb_${version}_linux_${arch}.tar.gz"
  tar --sort=name --mtime="@$source_epoch" --owner=0 --group=0 --numeric-owner \
    -C "$staging" -cf - garm-provider-timeweb README.md LICENSE BUILDINFO examples docs \
    | gzip -n > "$archive"
  printf 'Built %s\n' "$archive"
done

(
  cd dist
  sha256sum "garm-provider-timeweb_${version}_linux_amd64.tar.gz" \
    "garm-provider-timeweb_${version}_linux_arm64.tar.gz" > SHA256SUMS
)
