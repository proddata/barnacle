#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 path/to/hermit.rpm" >&2
    exit 2
fi

rpm_dir=$(CDPATH= cd -- "$(dirname -- "$1")" && pwd)
rpm_path="$rpm_dir/$(basename -- "$1")"
[ -f "$rpm_path" ] || { echo "RPM not found: $rpm_path" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo 'docker is required' >&2; exit 1; }

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
container="hermit-fedora-service-$$"
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
docker cp "$rpm_path" "$container:/var/tmp/hermit.rpm"
docker exec "$container" dnf install -y /var/tmp/hermit.rpm >/dev/null
docker exec "$container" rpm -V hermit
docker exec "$container" systemd-analyze verify hermit.service
docker exec "$container" systemctl start hermit

wait_health() {
    port=$1
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
        if docker exec "$container" curl --silent --fail "http://127.0.0.1:$port/healthz" >/dev/null; then
            return 0
        fi
        sleep 1
    done
    docker exec "$container" systemctl status hermit --no-pager || true
    return 1
}

wait_health 8080
docker exec "$container" sh -c '
    set -eu
    test "$(systemctl show -P Restart hermit)" = on-failure
    test "$(systemctl show -P MemoryHigh hermit)" = 201326592
    test "$(systemctl show -P MemoryMax hermit)" = 268435456
    test "$(systemctl show -P MemorySwapMax hermit)" = 0
    test "$(systemctl show -P DynamicUser hermit)" = yes
'

old_pid=$(docker exec "$container" systemctl show -P MainPID hermit)
docker exec "$container" kill -KILL "$old_pid"
restarted=false
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    new_pid=$(docker exec "$container" systemctl show -P MainPID hermit)
    if [ "$new_pid" != 0 ] && [ "$new_pid" != "$old_pid" ] && \
       docker exec "$container" curl --silent --fail http://127.0.0.1:8080/healthz >/dev/null; then
        restarted=true
        break
    fi
    sleep 1
done
[ "$restarted" = true ] || { echo 'systemd did not restart Hermit after SIGKILL' >&2; exit 1; }
docker exec "$container" sh -c 'test "$(systemctl show -P NRestarts hermit)" -ge 1'

docker exec "$container" sed -i 's/127.0.0.1:8080/127.0.0.1:18080/' /etc/sysconfig/hermit
docker exec "$container" systemctl restart hermit
wait_health 18080
if docker exec "$container" curl --silent --fail --max-time 2 http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'old listen address remained active after config change' >&2
    exit 1
fi
docker exec "$container" sed -i 's/127.0.0.1:18080/127.0.0.1:8080/' /etc/sysconfig/hermit
docker exec "$container" systemctl restart hermit
wait_health 8080

# The packaged fixed-user drop-in is opt-in. Verify a group-restricted key.
docker exec "$container" systemctl stop hermit
docker exec "$container" sh -c '
    set -eu
    groupadd --system hermit
    groupadd --system hermit-service-keys
    useradd --system --gid hermit --home-dir /nonexistent --shell /sbin/nologin hermit
    install -d -m 0750 -o root -g hermit-service-keys /run/hermit/service-keys
    install -d -m 0755 /etc/pki/hermit /etc/systemd/system/hermit.service.d
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
        -subj /CN=hermit-fedora-service-test \
        -keyout /run/hermit/service-keys/client.key \
        -out /etc/pki/hermit/ca.pem >/dev/null 2>&1
    chown root:hermit-service-keys /run/hermit/service-keys/client.key
    chmod 0640 /run/hermit/service-keys/client.key
    ! runuser -u hermit -g hermit -- test -r /run/hermit/service-keys/client.key
    cp /usr/share/hermit/hermit-key-access.conf /etc/systemd/system/hermit.service.d/key-access.conf
    cat > /etc/systemd/system/hermit.service.d/99-key-check.conf <<EOF
[Service]
ExecStartPre=/usr/bin/test -r /run/hermit/service-keys/client.key
EOF
    printf "\nHERMIT_PG_CA_FILE=/etc/pki/hermit/ca.pem\n" >> /etc/sysconfig/hermit
    systemctl daemon-reload
    systemd-analyze verify hermit.service
    systemctl restart hermit
'
wait_health 8080
docker exec "$container" sh -c '
    set -eu
    test "$(systemctl show -P DynamicUser hermit)" = no
    test "$(systemctl show -P User hermit)" = hermit
    test "$(systemctl show -P Group hermit)" = hermit
    test "$(systemctl show -P SupplementaryGroups hermit)" = hermit-service-keys
    case "$(systemctl show -P ReadOnlyPaths hermit)" in
        *"/run/hermit/service-keys"*) ;;
        *) exit 1 ;;
    esac
    case "$(systemctl show -P ReadOnlyPaths hermit)" in
        *"/etc/pki/hermit"*) ;;
        *) exit 1 ;;
    esac
    test "$(systemctl show -P NoNewPrivileges hermit)" = yes
    test "$(systemctl show -P PrivateTmp hermit)" = yes
    test "$(systemctl show -P ProtectHome hermit)" = yes
    test "$(systemctl show -P ProtectSystem hermit)" = strict
    test "$(systemctl show -P MemoryHigh hermit)" = 201326592
    test "$(systemctl show -P MemoryMax hermit)" = 268435456
    test "$(systemctl show -P MemorySwapMax hermit)" = 0
'

docker exec "$container" systemctl stop hermit
docker exec "$container" sh -c 'test "$(systemctl is-active hermit)" = inactive'
if docker exec "$container" curl --silent --fail --max-time 2 http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'Hermit remained reachable after systemctl stop' >&2
    exit 1
fi

echo 'Fedora RPM: systemd startup, memory policy, crash restart, config reload, restricted key access, and stop passed'
