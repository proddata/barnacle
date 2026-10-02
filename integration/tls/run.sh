#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_dir"
tls_dir=$(mktemp -d)
mkdir -p "$tls_dir/certs"
chmod 755 "$tls_dir" "$tls_dir/certs"
export HERMIT_TLS_DIR="$tls_dir/certs"
existing_hermit=$(docker compose ps -q hermit)

cleanup() {
    docker compose -f compose.yaml -f integration/tls/compose.yaml stop haproxy >/dev/null 2>&1 || true
    docker compose -f compose.yaml -f integration/tls/compose.yaml rm -f haproxy >/dev/null 2>&1 || true
    if [ -z "$existing_hermit" ]; then
        docker compose down >/dev/null 2>&1 || true
    fi
    rm -rf "$tls_dir"
}
trap cleanup EXIT HUP INT TERM

openssl genrsa -out "$tls_dir/ca.key" 2048 >/dev/null 2>&1
openssl req -x509 -new -sha256 -key "$tls_dir/ca.key" -out "$tls_dir/ca.crt" \
    -days 2 -subj '/CN=Hermit Test CA' \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' >/dev/null 2>&1
openssl req -new -newkey rsa:2048 -nodes -keyout "$tls_dir/server.key" \
    -out "$tls_dir/server.csr" -subj '/CN=localhost' >/dev/null 2>&1
cat > "$tls_dir/server.ext" <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:localhost,IP:127.0.0.1
EOF
openssl x509 -req -sha256 -in "$tls_dir/server.csr" -CA "$tls_dir/ca.crt" \
    -CAkey "$tls_dir/ca.key" -CAcreateserial -out "$tls_dir/server.crt" \
    -days 2 -extfile "$tls_dir/server.ext" >/dev/null 2>&1
cat "$tls_dir/server.crt" "$tls_dir/server.key" > "$tls_dir/certs/server.pem"
chmod 644 "$tls_dir/certs/server.pem"

docker compose -f compose.yaml -f integration/tls/compose.yaml up --build -d haproxy
ready=false
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl --silent --show-error --fail --cacert "$tls_dir/ca.crt" \
        https://127.0.0.1:8443/healthz >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 1
done
if [ "$ready" != true ]; then
    docker compose -f compose.yaml -f integration/tls/compose.yaml logs haproxy
    echo 'HAProxy TLS endpoint did not become ready' >&2
    exit 1
fi

HERMIT_BASE_URL=https://127.0.0.1:8443 \
NODE_EXTRA_CA_CERTS="$tls_dir/ca.crt" \
TEST_DATABASE_URL=postgres://hermit:hermit_dev_password@localhost:5432/hermit \
npm test --prefix integration
