#!/bin/sh
set -eu

command -v rpmbuild >/dev/null 2>&1 || { echo 'rpmbuild is required (dnf install rpm-build systemd-rpm-macros)' >&2; exit 1; }

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
version=$(awk '/^Version:/ { print $2; exit }' "$repo_dir/packaging/rpm/hermit.spec")
case "$version" in ''|*[!0-9A-Za-z.~_+-]*) echo 'invalid RPM version' >&2; exit 1;; esac

build_dir=$(mktemp -d)
cleanup() { rm -rf "$build_dir"; }
trap cleanup EXIT HUP INT TERM
for dir in BUILD BUILDROOT RPMS SOURCES SPECS SRPMS; do
    mkdir -p "$build_dir/$dir"
done
source_dir="$build_dir/SOURCES/hermit-$version"
mkdir -p "$source_dir/packaging/rpm"

cd "$repo_dir"
go mod download
cp go.mod go.sum ./*.go README.md LICENSE THIRD-PARTY-NOTICES.md "$source_dir/"
cp -R web "$source_dir/"
cp -R internal "$source_dir/"
cp -R THIRD-PARTY-LICENSES "$source_dir/"
cp packaging/rpm/hermit.service packaging/rpm/hermit.sysconfig "$source_dir/packaging/rpm/"
tar -C "$build_dir/SOURCES" -czf "$build_dir/SOURCES/hermit-$version.tar.gz" "hermit-$version"
cp packaging/rpm/hermit.spec "$build_dir/SPECS/"

rpmbuild -bb --define "_topdir $build_dir" "$build_dir/SPECS/hermit.spec"
mkdir -p "$repo_dir/dist"
find "$build_dir/RPMS" -type f -name 'hermit-*.rpm' -exec cp {} "$repo_dir/dist/" \;
echo "RPM written to $repo_dir/dist/"
