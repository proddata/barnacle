#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
mkdir -p "$repo_dir/dist"
for arch in amd64 arm64; do
    echo "Building linux/$arch"
    (cd "$repo_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "dist/hermit-linux-$arch" .)
done
(cd "$repo_dir/dist" && if command -v sha256sum >/dev/null 2>&1; then sha256sum hermit-linux-amd64 hermit-linux-arm64 > SHA256SUMS; else shasum -a 256 hermit-linux-amd64 hermit-linux-arm64 > SHA256SUMS; fi)
echo "Linux binaries and checksums written to $repo_dir/dist"
