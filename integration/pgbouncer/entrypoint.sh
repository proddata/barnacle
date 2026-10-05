#!/bin/sh
set -eu
: "${POOL_MODE:?set POOL_MODE}"

cat > /tmp/pgbouncer.ini <<EOF
[databases]
hermit = host=postgres port=5432 dbname=hermit

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = plain
auth_file = /tmp/userlist.txt
pool_mode = $POOL_MODE
max_prepared_statements = 0
default_pool_size = ${POOL_SIZE:-2}
server_idle_timeout = 1
max_client_conn = 100
pidfile = /tmp/pgbouncer.pid
EOF
if [ "${TLS_MODE:-}" = require ]; then
    cp /pgcerts/server.crt /tmp/server.crt
    cp /pgcerts/server.key /tmp/server.key
    chown pgbouncer:pgbouncer /tmp/server.crt /tmp/server.key
    chmod 0600 /tmp/server.key
    cat >> /tmp/pgbouncer.ini <<EOF
client_tls_sslmode = require
client_tls_cert_file = /tmp/server.crt
client_tls_key_file = /tmp/server.key
server_tls_sslmode = verify-full
server_tls_ca_file = /pgcerts/ca.crt
EOF
fi
printf '"hermit" "hermit_dev_password"\n' > /tmp/userlist.txt
chown pgbouncer:pgbouncer /tmp/pgbouncer.ini /tmp/userlist.txt
exec su-exec pgbouncer pgbouncer /tmp/pgbouncer.ini
