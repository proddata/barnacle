#!/bin/sh
set -eu

command -v rpmbuild >/dev/null 2>&1 || { echo 'rpmbuild is required (dnf install rpm-build systemd-rpm-macros)' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo 'python3 is required' >&2; exit 1; }

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
version=$(awk '/^Version:/ { print $2; exit }' "$repo_dir/packaging/rpm/hermit.spec")
case "$version" in ''|*[!0-9A-Za-z.~_+-]*) echo 'invalid RPM version' >&2; exit 1;; esac

build_dir=$(mktemp -d)
cleanup() { rm -rf "$build_dir"; }
trap cleanup EXIT HUP INT TERM
for dir in BUILD BUILDROOT RPMS SOURCES SPECS SRPMS; do
    mkdir -p "$build_dir/$dir"
done
cd "$repo_dir"
if [ -n "${HERMIT_SOURCE_ARCHIVE_DIR:-}" ]; then
    cp "$HERMIT_SOURCE_ARCHIVE_DIR/hermit-$version.tar.gz" \
       "$HERMIT_SOURCE_ARCHIVE_DIR/hermit-$version-vendor.tar.gz" "$build_dir/SOURCES/"
else
    python3 packaging/release/source.py
    cp "dist/hermit-$version.tar.gz" "dist/hermit-$version-vendor.tar.gz" "$build_dir/SOURCES/"
fi
cp packaging/rpm/hermit.spec "$build_dir/SPECS/"

rpmbuild -bb --define "_topdir $build_dir" "$build_dir/SPECS/hermit.spec"
mkdir -p "$repo_dir/dist"
find "$build_dir/RPMS" -type f -name 'hermit-*.rpm' -exec cp {} "$repo_dir/dist/" \;
echo "RPM written to $repo_dir/dist/"
