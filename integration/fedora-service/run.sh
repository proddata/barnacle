#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 path/to/barnacle.rpm" >&2
    exit 2
fi

rpm_dir=$(CDPATH= cd -- "$(dirname -- "$1")" && pwd)
rpm_path="$rpm_dir/$(basename -- "$1")"
[ -f "$rpm_path" ] || { echo "RPM not found: $rpm_path" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo 'docker is required' >&2; exit 1; }

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
container="barnacle-fedora-service-$$"
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT HUP INT TERM

image=$(docker build --quiet "$script_dir")
docker run --detach --name "$container" --privileged --cgroupns=private \
    --tmpfs /run --tmpfs /tmp "$image" >/dev/null

for attempt in 1 2 3 4 5 6 7 8 9 10; do
    if docker exec "$container" systemctl is-system-running --quiet >/dev/null 2>&1; then break; fi
    sleep 1
done
docker exec "$container" systemctl is-system-running --quiet

# /tmp is a systemd tmpfs; docker cp must use a path on the container rootfs.
docker cp "$rpm_path" "$container:/var/tmp/barnacle.rpm"
docker exec "$container" dnf install -y /var/tmp/barnacle.rpm >/dev/null
docker exec "$container" rpm -V barnacle
docker exec "$container" systemd-analyze verify barnacle.service
docker exec "$container" systemctl start barnacle

wait_health() {
    port=$1
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
        if docker exec "$container" curl --silent --fail "http://127.0.0.1:$port/healthz" >/dev/null; then
            return 0
        fi
        sleep 1
    done
    docker exec "$container" systemctl status barnacle --no-pager || true
    return 1
}

wait_health 8080
docker exec "$container" sh -c '
    set -eu
    test "$(systemctl show -P Restart barnacle)" = on-failure
    test "$(systemctl show -P MemoryHigh barnacle)" = 201326592
    test "$(systemctl show -P MemoryMax barnacle)" = 268435456
    test "$(systemctl show -P MemorySwapMax barnacle)" = 0
    test "$(systemctl show -P DynamicUser barnacle)" = yes
'

old_pid=$(docker exec "$container" systemctl show -P MainPID barnacle)
docker exec "$container" kill -KILL "$old_pid"
restarted=false
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    new_pid=$(docker exec "$container" systemctl show -P MainPID barnacle)
    if [ "$new_pid" != 0 ] && [ "$new_pid" != "$old_pid" ] && \
       docker exec "$container" curl --silent --fail http://127.0.0.1:8080/healthz >/dev/null; then
        restarted=true
        break
    fi
    sleep 1
done
[ "$restarted" = true ] || { echo 'systemd did not restart Barnacle after SIGKILL' >&2; exit 1; }
docker exec "$container" sh -c 'test "$(systemctl show -P NRestarts barnacle)" -ge 1'

docker exec "$container" sed -i 's/127.0.0.1:8080/127.0.0.1:18080/' /etc/sysconfig/barnacle
docker exec "$container" systemctl restart barnacle
wait_health 18080
if docker exec "$container" curl --silent --fail --max-time 2 http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'old listen address remained active after config change' >&2
    exit 1
fi
docker exec "$container" sed -i 's/127.0.0.1:18080/127.0.0.1:8080/' /etc/sysconfig/barnacle
docker exec "$container" systemctl restart barnacle
wait_health 8080

# A node agent configures the service without touching the packaged files: a
# drop-in adds a second settings file, and a few unit lines are overridden.
# Everything the drop-in does not mention must keep the packaged value.
docker exec "$container" sh -c '
    set -eu
    install -d -m 0755 /etc/barnacle /etc/systemd/system/barnacle.service.d
    cat > /etc/barnacle/barnacle.env <<EOF
BARNACLE_LISTEN=127.0.0.1:9682
BARNACLE_MAX_CONNECTIONS=16
EOF
    cat > /etc/systemd/system/barnacle.service.d/10-node-agent.conf <<EOF
[Service]
EnvironmentFile=/etc/barnacle/barnacle.env
MemoryMax=384M
EOF
    systemctl daemon-reload
    systemd-analyze verify barnacle.service
    systemctl restart barnacle
'
wait_health 9682
if docker exec "$container" curl --silent --fail --max-time 2 http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'packaged listen address stayed active after the drop-in settings file overrode it' >&2
    exit 1
fi
docker exec "$container" sh -c '
    set -eu
    # The drop-in changed only the lines it names.
    test "$(systemctl show -P MemoryMax barnacle)" = 402653184
    # Everything else still comes from the packaged unit.
    test "$(systemctl show -P MemoryHigh barnacle)" = 201326592
    test "$(systemctl show -P MemorySwapMax barnacle)" = 0
    test "$(systemctl show -P Restart barnacle)" = on-failure
    test "$(systemctl show -P DynamicUser barnacle)" = yes
    test "$(systemctl show -P NoNewPrivileges barnacle)" = yes
    test "$(systemctl show -P ProtectSystem barnacle)" = strict
    # Both settings files are read, and the later one wins for a shared name.
    case "$(systemctl show -P EnvironmentFiles barnacle)" in
        *"/etc/sysconfig/barnacle"*"/etc/barnacle/barnacle.env"*) ;;
        *) exit 1 ;;
    esac
'
docker exec "$container" sh -c '
    set -eu
    rm /etc/systemd/system/barnacle.service.d/10-node-agent.conf /etc/barnacle/barnacle.env
    systemctl daemon-reload
    systemctl restart barnacle
'
wait_health 8080

# The packaged fixed-user drop-in is opt-in. Verify a group-restricted key.
docker exec "$container" systemctl stop barnacle
docker exec "$container" sh -c '
    set -eu
    groupadd --system barnacle
    groupadd --system barnacle-service-keys
    useradd --system --gid barnacle --home-dir /nonexistent --shell /sbin/nologin barnacle
    install -d -m 0750 -o root -g barnacle-service-keys /run/barnacle/service-keys
    install -d -m 0755 /etc/pki/barnacle /etc/systemd/system/barnacle.service.d
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
        -subj /CN=barnacle-fedora-service-test \
        -keyout /run/barnacle/service-keys/client.key \
        -out /etc/pki/barnacle/ca.pem >/dev/null 2>&1
    chown root:barnacle-service-keys /run/barnacle/service-keys/client.key
    chmod 0640 /run/barnacle/service-keys/client.key
    ! runuser -u barnacle -g barnacle -- test -r /run/barnacle/service-keys/client.key
    cp /usr/share/barnacle/barnacle-key-access.conf /etc/systemd/system/barnacle.service.d/key-access.conf
    cat > /etc/systemd/system/barnacle.service.d/99-key-check.conf <<EOF
[Service]
ExecStartPre=/usr/bin/test -r /run/barnacle/service-keys/client.key
EOF
    printf "\nBARNACLE_PG_CA_FILE=/etc/pki/barnacle/ca.pem\n" >> /etc/sysconfig/barnacle
    systemctl daemon-reload
    systemd-analyze verify barnacle.service
    systemctl restart barnacle
'
wait_health 8080
docker exec "$container" sh -c '
    set -eu
    test "$(systemctl show -P DynamicUser barnacle)" = no
    test "$(systemctl show -P User barnacle)" = barnacle
    test "$(systemctl show -P Group barnacle)" = barnacle
    test "$(systemctl show -P SupplementaryGroups barnacle)" = barnacle-service-keys
    case "$(systemctl show -P ReadOnlyPaths barnacle)" in
        *"/run/barnacle/service-keys"*) ;;
        *) exit 1 ;;
    esac
    case "$(systemctl show -P ReadOnlyPaths barnacle)" in
        *"/etc/pki/barnacle"*) ;;
        *) exit 1 ;;
    esac
    test "$(systemctl show -P NoNewPrivileges barnacle)" = yes
    test "$(systemctl show -P PrivateTmp barnacle)" = yes
    test "$(systemctl show -P ProtectHome barnacle)" = yes
    test "$(systemctl show -P ProtectSystem barnacle)" = strict
    test "$(systemctl show -P MemoryHigh barnacle)" = 201326592
    test "$(systemctl show -P MemoryMax barnacle)" = 268435456
    test "$(systemctl show -P MemorySwapMax barnacle)" = 0
'

docker exec "$container" systemctl stop barnacle
docker exec "$container" sh -c 'test "$(systemctl is-active barnacle)" = inactive'
if docker exec "$container" curl --silent --fail --max-time 2 http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'Barnacle remained reachable after systemctl stop' >&2
    exit 1
fi

echo 'Fedora RPM: systemd startup, memory policy, crash restart, config reload, node-agent drop-in, restricted key access, and stop passed'
