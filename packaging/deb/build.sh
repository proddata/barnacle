#!/bin/sh
set -eu

command -v dpkg-deb >/dev/null 2>&1 || { echo 'dpkg-deb is required' >&2; exit 1; }
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
version=${1:-$(awk '/^Version:/ { print $2; exit }' "$repo_dir/packaging/rpm/hermit.spec")}
case "$version" in ''|*[!0-9A-Za-z.+~:-]*) echo 'invalid Debian version' >&2; exit 1;; esac
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
mkdir -p "$repo_dir/dist"

for arch in amd64 arm64; do
    binary="$repo_dir/dist/hermit-linux-$arch"
    test -f "$binary" || { echo "missing $binary; run packaging/release/build.sh first" >&2; exit 1; }
    root="$work_dir/hermit-$arch"
    mkdir -p "$root/DEBIAN" "$root/usr/bin" "$root/usr/lib/systemd/system" "$root/etc/default" "$root/usr/share/doc/hermit/THIRD-PARTY-LICENSES"
    install -m 0755 "$binary" "$root/usr/bin/hermit"
    install -m 0644 "$repo_dir/packaging/deb/hermit.service" "$root/usr/lib/systemd/system/hermit.service"
    install -m 0644 "$repo_dir/packaging/deb/hermit.default" "$root/etc/default/hermit"
    install -m 0644 "$repo_dir/LICENSE" "$root/usr/share/doc/hermit/copyright"
    install -m 0644 "$repo_dir/THIRD-PARTY-NOTICES.md" "$root/usr/share/doc/hermit/THIRD-PARTY-NOTICES.md"
    cp -R "$repo_dir/THIRD-PARTY-LICENSES/." "$root/usr/share/doc/hermit/THIRD-PARTY-LICENSES/"
    cat > "$root/DEBIAN/control" <<EOF
Package: hermit
Version: $version
Section: database
Priority: optional
Architecture: $arch
Maintainer: Hermit contributors
Depends: systemd
Description: HTTP and WebSocket gateway for PostgreSQL
 Hermit exposes a Neon-compatible subset of SQL over HTTP and PostgreSQL
 sessions over WebSocket for a configured PostgreSQL or pooler upstream.
EOF
    echo /etc/default/hermit > "$root/DEBIAN/conffiles"
    cat > "$root/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload || true; fi
EOF
    cat > "$root/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload || true; fi
EOF
    chmod 0755 "$root/DEBIAN/postinst" "$root/DEBIAN/postrm"
    dpkg-deb --build --root-owner-group "$root" "$repo_dir/dist/hermit_${version}_${arch}.deb"
done
