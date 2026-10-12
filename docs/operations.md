# Package operations

For upstream routing, TLS, and production checks, start with the [deployment guide](deployment.md).

## Ubuntu and Debian service

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

### Configuring the service from a node agent

A platform that manages the node, such as an agent that writes settings per service, should not write its own unit file. Use the packaged unit and add two small files, so the memory limits, restart policy and sandboxing arrive and update with the package:

```ini
# /etc/systemd/system/barnacle.service.d/10-node-agent.conf
[Service]
EnvironmentFile=/etc/barnacle/barnacle.env
```

```sh
# /etc/barnacle/barnacle.env
BARNACLE_LISTEN=127.0.0.1:9682
BARNACLE_PG_ADDR=127.0.0.1:5432
BARNACLE_PG_SSLMODE=disable
```

How the layers combine:

- A drop-in changes only the lines it names. Everything else, including `MemoryMax`, `Restart=`, `NoNewPrivileges=` and `ProtectSystem=`, keeps the packaged value. To change a limit, name just that line (for example `MemoryMax=384M`) and set `GOMEMLIMIT` in proportion, as described below.
- A drop-in `EnvironmentFile=` is added after the packaged `/etc/sysconfig/barnacle`. When both files define a variable, the later file wins; a variable only the packaged file defines (such as `GOMEMLIMIT`) still applies.
- A `BARNACLE_*` variable that neither file defines uses Barnacle's built-in default. Write only the values that differ.
- Leave `/etc/sysconfig/barnacle` alone: it is a package-owned config file and an upgrade may add a `.rpmnew` next to your edits.
- Run `systemctl daemon-reload` after changing a drop-in, and restart the service after changing a settings file. The Fedora service test covers this layout.

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

The unit restarts Barnacle after a crash or OOM kill and waits five seconds between attempts. It never gives up (`StartLimitIntervalSec=0`), so a node recovers on its own after repeated OOM kills; a service that cannot start at all, for example with an invalid setting, restarts every five seconds until fixed. Alert on `NRestarts` and on unit health, and stop the service explicitly to take it down. On SIGTERM, Barnacle stops accepting requests, gives active HTTP queries up to ten seconds to finish, sends WebSocket close code 1001, and cancels running PostgreSQL work for those sessions. The unit allows 15 seconds before forcing the process to stop. Its cgroup begins throttling at 192 MiB and has a hard 256 MiB memory limit with no swap; `GOMEMLIMIT=160MiB` asks Go to collect earlier. `OOMScoreAdjust=500` makes Barnacle a more likely victim than an unadjusted PostgreSQL process if the whole host runs out of memory. These are starter limits for a small proxy; measure your query sizes and concurrent WebSocket sessions before raising them. The hard limit keeps Barnacle's memory use bounded, but a request that needs more memory can fail and drop its connection.

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

Also adjust `GOMEMLIMIT` in `/etc/sysconfig/barnacle` when changing `MemoryMax`, and keep the two in proportion (`GOMEMLIMIT` about 5/8 of `MemoryMax`). When `BARNACLE_HTTP_MAX_MEMORY_MIB` is unset, Barnacle derives the heap budget for `/sql` request bodies and buffered results as `GOMEMLIMIT` less 64 MiB (96 MiB for the packaged unit), so raising `MemoryMax` and `GOMEMLIMIT` together raises it, and larger services give a bigger share of their limit to the budget; the startup log states the value. `BARNACLE_MAX_CONNECTIONS=32` limits the PostgreSQL backends Barnacle can create across both transports; tune it below PostgreSQL's available connection budget. PostgreSQL still needs its own work memory and statement timeout settings.

## Validating configuration

`barnacle -check-config` reads the same `BARNACLE_*` environment as the service, validates every setting (including certificate and CA files), prints `configuration ok` and exits 0 without opening a listener. On invalid input it logs every problem at once and exits 1. A platform that generates the environment can run it in a test or as a unit `ExecStartPre=` to catch a bad setting before a restart. By default an unrecognized `BARNACLE_*` name is ignored, so a typo leaves the default in place. Add `-strict` (to `-check-config` or to the service itself) to fail on any `BARNACLE_*` variable the server does not read. Only use it where the environment is dedicated to Barnacle, because tooling variables such as `BARNACLE_BASE_URL` in the integration scripts would also be rejected.

## Migrating an installed Hermit service

Barnacle uses a separate package and systemd unit. Stop and disable `hermit.service` before starting `barnacle.service` on the same listen address. Copy the deployment settings from `/etc/default/hermit` or `/etc/sysconfig/hermit` into the Barnacle environment file, change each `HERMIT_` prefix to `BARNACLE_`, and review certificate, key, and service drop-in paths. Keep the old package and configuration until the new service passes deployment checks. The database and its roles do not need renaming.
