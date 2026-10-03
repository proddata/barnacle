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

docker exec "$container" systemctl stop hermit
docker exec "$container" sh -c 'test "$(systemctl is-active hermit)" = inactive'
if docker exec "$container" curl --silent --fail --max-time 2 http://127.0.0.1:8080/healthz >/dev/null; then
    echo 'Hermit remained reachable after systemctl stop' >&2
    exit 1
fi

echo 'Fedora RPM: systemd startup, memory policy, crash restart, config reload, and stop passed'
