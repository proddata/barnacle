# Package operations

For upstream routing, TLS, and production checks, start with the [deployment guide](deployment.md).

## Ubuntu and Debian service

If migrating from an installed Hermit package, Barnacle is a separate package and systemd unit. Stop and disable `hermit.service` before starting `barnacle.service` on the same listen address. Copy the deployment settings from `/etc/default/hermit` or `/etc/sysconfig/hermit` into the new Barnacle environment file, changing each `HERMIT_` prefix to `BARNACLE_`; review paths for certificates, keys, and service drop-ins. Keep the old package and configuration until the new service has passed deployment checks, then remove them through the package manager. The database and its roles do not need renaming for an installed deployment.

Install the `.deb` for your architecture from CI or [build it locally](development.md#release-binaries-container-and-debian-package), then edit `/etc/default/barnacle` and enable the service:

```sh
sudo apt install ./dist/barnacle_0.1.0_amd64.deb # example for amd64
sudoedit /etc/default/barnacle
sudo systemctl enable --now barnacle
```

The package supplies the same basic systemd hardening as the Fedora service below. Set `BARNACLE_PG_ADDR` and the PostgreSQL CA for the intended deployment before serving traffic.

## Fedora service

Install an RPM from CI or [build one locally](development.md#fedora-rpm):

```sh
sudo dnf install ./dist/barnacle-*.rpm
sudoedit /etc/sysconfig/barnacle
sudo systemctl enable --now barnacle
```

The package installs `/usr/bin/barnacle`, a systemd service, and `/etc/sysconfig/barnacle`. The service listens on `127.0.0.1:8080` and connects to PostgreSQL on `127.0.0.1:5432` by default, so set the latter in the config file if PostgreSQL is elsewhere. It includes Barnacle's [Apache-2.0 license](../LICENSE) and the [third-party license inventory](../THIRD-PARTY-NOTICES.md).

### Fixed-user key-access variant

The RPM also ships an opt-in [key-access drop-in](../packaging/rpm/barnacle-key-access.conf). Its fixed `barnacle` system user and group replace `DynamicUser=yes`; membership in `barnacle-service-keys` lets the service read group-restricted keys under `/run/barnacle/service-keys/`. It explicitly mounts that directory and `/etc/pki/barnacle/` read-only. Create the user, its primary group, and the key-access group before starting the variant. If the host stores keys or certificates elsewhere, set `ReadOnlyPaths=` to those directories and `SupplementaryGroups=` to the key's existing group. Ensure directory traversal permissions and key group ownership also permit reading. The packaged unit's `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, restart policy, and memory limits still apply.

For a manual installation after provisioning those accounts and directories:

```sh
sudo install -d -m 0755 /etc/systemd/system/barnacle.service.d
sudo install -m 0644 /usr/share/barnacle/barnacle-key-access.conf /etc/systemd/system/barnacle.service.d/key-access.conf
sudoedit /etc/sysconfig/barnacle
sudo systemctl daemon-reload
sudo systemctl restart barnacle
```

Keep `BARNACLE_PG_ADDR`, `BARNACLE_PG_TLS_SERVER_NAME`, `BARNACLE_PG_CA_FILE`, `BARNACLE_LISTEN`, and `BARNACLE_TRUSTED_PROXIES` in the service environment file; the drop-in does not set deployment-specific values. Set `BARNACLE_PG_CA_FILE` to the appropriate CA bundle path when Barnacle verifies the upstream PostgreSQL certificate. Set `BARNACLE_TRUSTED_PROXIES` only to HAProxy's immediate source IP when it sets the forwarded scheme, and restrict direct access to Barnacle. The Fedora service test checks the effective unit settings and a group-restricted key read.

The unit restarts Barnacle after a crash or OOM kill, waits five seconds between attempts, and stops after five starts in one minute. On SIGTERM, Barnacle stops accepting requests, gives active HTTP queries up to ten seconds to finish, sends WebSocket close code 1001, and cancels running PostgreSQL work for those sessions. The unit allows 15 seconds before forcing the process to stop. Its cgroup begins throttling at 192 MiB and has a hard 256 MiB memory limit with no swap; `GOMEMLIMIT=160MiB` asks Go to collect earlier. `OOMScoreAdjust=500` makes Barnacle a more likely victim than an unadjusted PostgreSQL process if the whole host runs out of memory. These are starter limits for a small proxy; measure your query sizes and concurrent WebSocket sessions before raising them. The hard limit keeps Barnacle's memory use bounded, but a request that needs more memory can fail and drop its connection.

Inspect restarts and memory use with `systemctl status barnacle`, `journalctl -u barnacle`, and `systemctl show barnacle -p MemoryCurrent -p MemoryPeak -p NRestarts`. Tune cgroup limits without editing the packaged unit:

```sh
sudo systemctl edit barnacle
```

In the editor, add:

```ini
[Service]
MemoryHigh=384M
MemoryMax=512M
```

Then restart the service:

```sh
sudo systemctl restart barnacle
```

Also adjust `GOMEMLIMIT` in `/etc/sysconfig/barnacle` when changing `MemoryMax`. If repeated failures hit the start limit, fix the cause and run `sudo systemctl reset-failed barnacle && sudo systemctl start barnacle`. `BARNACLE_MAX_CONNECTIONS=32` limits the PostgreSQL backends Barnacle can create across both transports; tune it below PostgreSQL's available connection budget. PostgreSQL still needs its own work memory and statement timeout settings.
