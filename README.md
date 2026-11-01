# CasaOS-AppManagement

> **Not affiliated with IceWhale.** An independent, community-maintained distribution of CasaOS, not produced or endorsed by Shanghai IceWhale Technology Limited. CASAOS is their trademark, used here only to say what this is a release of. The original project is [IceWhaleTech/CasaOS](https://github.com/IceWhaleTech/CasaOS); report problems with this distribution at [ReCasaOS/CasaOS/issues](https://github.com/ReCasaOS/CasaOS/issues).

The service that installs and runs apps on a CasaOS host. It is the Docker Compose layer of the system: it keeps the app store catalogues, turns a store entry into a `docker-compose.yml`, runs Compose against the local Docker daemon, and reports container status, logs and available updates back to the dashboard.

This repository is part of **ReCasaOS**, a maintained release of the project after upstream [IceWhaleTech](https://github.com/IceWhaleTech/CasaOS-AppManagement) stopped shipping in 2025. It descends from [alvins82's fork](https://github.com/alvins82/CasaOS-AppManagement), whose Docker SDK bump is what keeps CasaOS installable on a current daemon.

## What it does

The service listens on a random loopback port, writes that address to `/var/run/casaos/app-management.url`, and registers its paths with CasaOS-Gateway:

| Path | What is behind it |
|---|---|
| `/v2/app_management` | the current API |
| `/v1/apps`, `/v1/container`, `/v1/app-categories` | the v1 API, kept for older clients |
| `/doc/v2/app_management`, `/v1doc/v1/app_management` | the OpenAPI specs and their viewer |

Under `/v2/app_management`: `/appstore` registers and lists stores, `/apps` serves the merged catalogue (with `/apps/upgradable` and `/apps/{id}/compose`), `/categories` the category list and its counts, `/compose` and `/compose/{id}` install an app and then apply settings, update, start, stop or uninstall it (`/status`, `/containers`, `/logs`, `/healthcheck`), `/container/{id}` reports and health-checks a single container, and `/image` pulls images ahead of an install. `/compose/{id}/env` reads and replaces the `.env` next to that app's `docker-compose.yml` and re-creates the app so the new values apply. The full list is `api/app_management/openapi.yaml`; the Go server, types and MessageBus client are generated from it and are not committed.

Two jobs run on a schedule from `main.go`: the store catalogues are refreshed every 10 minutes, and every 15 seconds a sweep starts apps Docker abandoned at boot because their storage was not mounted yet.

On a host:

- `/etc/casaos/app-management.conf` — paths and the app store URLs, written from `build/sysroot/etc/casaos/app-management.conf.sample` on first start
- `/etc/casaos/env` — variables injected into every app
- `/var/lib/casaos/appstore` — the downloaded catalogues
- `/var/lib/casaos/apps/<name>/docker-compose.yml` — one directory per installed app; this is the file the settings page and the compose editor write
- `/var/log/casaos/app-management.log`
- `casaos-app-management.service`, started after `docker.service` and `casaos-message-bus.service`

There is no database. An installed app is its compose file plus whatever the Docker daemon reports about it.

## Install

Components are not installed on their own. The distribution is installed and upgraded with one command:

```sh
curl -fsSL https://github.com/ReCasaOS/CasaOS-Install/releases/latest/download/install.sh | sudo bash
```

What a release contains, and how it is built, is described in [CasaOS-Install](https://github.com/ReCasaOS/CasaOS-Install#readme).

## What this fork changed

From alvins82:

- **Docker 26 and later.** The SDKs were pinned to docker/cli and docker/docker v24, compose/v2 v2.23 and compose-go v1, which is why CasaOS stopped working against a current daemon. They are now v26.1.0, v2.27.0 and compose-go/v2 v2.1.0; the client already negotiated the API version with the daemon, and `pkg/docker/api_negotiation_test.go` now asserts it still does. This is the change that made the fork necessary. Offered upstream as [#217](https://github.com/IceWhaleTech/CasaOS-AppManagement/pull/217), still unmerged.
- **Apps abandoned at boot.** Docker gives up on a container whose bind mount does not exist yet, which happens on every reboot when apps live on a disk mounted after `docker.service`. A recovery sweep starts those apps once their storage appears, and leaves alone the ones the user stopped.

Here:

- **An app can keep its secrets in a `.env`.** An installed app may carry a `.env` next to its `docker-compose.yml`, edited from the dashboard through `GET`/`PUT /v2/app_management/compose/{id}/env`: `PUT` replaces the whole file (an empty body removes it) and re-creates the app so the new values apply. A reference to one of its keys (`${KEY}`, `$KEY`, `${KEY:-default}`), or to a key the runtime defines (`$AppID`, `${TZ}`, `$PUID`, `${PGID}`), now survives every settings round trip and the App Store update as written, where before the first save or update baked the resolved value into the compose file and every later `.env` edit was a no-op; the key has to be in `.env` when the compose is saved, or the reference is baked as a literal. It is kept in `environment` and every other free-text field (`image`, `command`, `labels`, `env_file`, `x-casaos`...) and in the host side of `ports` and `volumes`, which come back in the long syntax; a typed field such as `cpus`, `mem_limit` or `privileged` cannot hold one, and the editing load (`GET /compose/{id}` as YAML) fails with a `500` naming that field instead of serving the resolved value. A `.env` that defines a key the runtime sets itself (`TZ`, `PUID`, `PGID`, `AppID`, the environment of the CasaOS process) is refused with a `400` naming the key, since the runtime's value wins over the file anyway. When the compose file does not load, the pull fails or the app fails to start after a `.env` change, the previous `.env` is put back together with the previous `docker-compose.yml` and the app is started from them again.
- **A `$` no longer doubles on every save.** The settings handler parsed the incoming YAML without interpolation and then re-escaped every `$`, so a value it had produced itself came back doubled: `$2a$12$…` became `$$2a$$12$$…`, then `$$$$2a$$$$12$$$$…`, and the container received a broken password hash. Parsing with interpolation on makes the round trip idempotent. Fixes [CasaOS#1988](https://github.com/IceWhaleTech/CasaOS/issues/1988).
- **Architecture filtering.** An app declaring `architectures: []` was hidden on every host, because an explicit empty list decodes to an empty slice rather than `nil` and only `nil` was treated as "runs anywhere" — which is what the dashboard already assumed. Category counts were computed from the unfiltered catalogue, so on arm hosts they were larger than the number of cards listed under them. Both now agree with the list. Supersedes the draft in [#202](https://github.com/IceWhaleTech/CasaOS-AppManagement/pull/202), which dereferenced that empty list and hardcoded amd64.
- **Releases.** The upstream workflow needed IceWhale's secrets and is gone. `.github/workflows/release.yml` runs the tests and publishes with GoReleaser on a `v*.*.*` tag using the default `GITHUB_TOKEN`. Our change to `.goreleaser.yaml` is one line, `release.github.owner`, now `ReCasaOS`, so the archive layout the installer consumes is unchanged.
- **No geo-IP at install time.** `build/scripts/migration/script.d` ran `__get_download_domain` at top level, curling `ipconfig.io/country` and then `ifconfig.io/country_code` to pick a download mirror by country. `install.sh` runs every script in that directory on every install and every upgrade, so both services were contacted each time, before the script had even decided whether a migration was due. On a current box it is not, though this repository is the one whose list still carries a reachable v0.4.5 entry. The migration tools now come from GitHub unconditionally, and the domain is a constant: it is not an environment knob either, because what it points at is downloaded and run as root without verification.
- **The compose lifecycle test is opt-in.** It skipped itself only when no Docker daemon was reachable, which is the wrong question: a CI runner has a daemon and no CasaOS behind it, so the test ran and failed on the missing app store. It now requires `CASAOS_INTEGRATION`.
- **Backups.** `POST /v2/app_management/compose/{id}/backup` copies an app to an rclone remote -- S3, SFTP, FTP, or anything else rclone speaks -- on demand or on a schedule with a retention (`/backup/schedules`, `/backup/runs`, `/backup/destinations`). None of the transport is new code: this distribution already installs rclone and runs it as a daemon on a unix socket, so the retries, the resume and the incremental comparison are its. What is written here is what rclone cannot know: which paths belong to the app and which of them are data (`/var/run/docker.sock` is a door, `/dev` and `/proc` are kernel interfaces, tmpfs is empty at every start, an anonymous volume has no name to restore it under -- each is named in the manifest with its reason), and how to hold the app still while it is copied, since a database copied while it writes produces a backup that looks fine and does not restore. What is about to be stopped is written down before the first container goes down and erased after the last comes back, and a note still there when the service starts is an app a killed backup left off, started again.

- **Backups with restic.** A destination can be switched to [restic](https://restic.net) (`PUT /backup/destinations/{name}/restic`, BSD-2-Clause): the same runs, schedules, retention and restores, but each run is stored in a restic repository on the destination (folder `casaos-restic`, reached through the same rclone remote), so a run costs what changed since the last one instead of another full copy, and files keep their owners and permissions. A copy becomes one restic snapshot tagged with the app, the stamp and the path it was asked to land at; a run is the snapshots that share a stamp, and deleting one forgets them and prunes. Opt-in per destination, and what the destination already holds stays restorable. The repository's password is made on the box, kept in `/var/lib/casaos/backups/restic/<name>.key` (root only) and **returned once** by the call that switches the destination: lose it and nothing can read the repository. Giving the call the password of a repository that is already there attaches the destination to it, which is how backups are reached again on another box.
- **A container CasaOS did not install can be identified and removed.** `GET /v2/app_management/container/{id}/detail` answers what the dashboard could not: image, command, restart policy, networks, published ports, host paths, named volumes with their size, environment. `DELETE /container/{id}` removes it, and removes named volumes only when asked and only when the daemon says nothing else uses them, re-deciding against the daemon at the moment of the request rather than against what the screen was drawn from. A container belonging to a compose project is refused: the app's own uninstall is how a stack goes.
- **An app whose compose file has no `x-casaos` is a first-class app.** Six defects on this distribution began with one written by hand: it stayed grey while every container ran, its status was computed and then dropped by the dashboard's own grid, clicking it opened nothing. The status no longer depends on a catalogue entry, and a web port is inferred from what the containers publish when the file names none -- the main service's, against a short list of ports images serve interfaces on, and nothing rather than a wrong link when none matches.
- **An update never replaces a floating tag with a fixed one.** An app tracking `develop` came back pinned to whatever version the catalogue named, because the decision read the catalogue's tag alone. Both references decide now; a digest is the opposite case, since `pull app@sha256:...` returns the same image for ever, so a digest-pinned app can only be updated by rewriting it.
- **One container at a time.** `POST /compose/{id}/container/{cid}/status` starts, stops or restarts one service of an app instead of the whole stack, and `GET /compose/{id}/container/{cid}/stats` samples its CPU and memory as `docker stats` reports them.
- **Resource history and alerts for every container** (see [docs/stats.md](docs/stats.md)). A sampler measures each container every 5 s and keeps per-minute avg/min/max for 7 days under `/var/lib/casaos/stats/app_management`. `GET /v2/app_management/stats` lists the containers (compose services as `app:<application>:<service>:<n>`, any other container as `container:<name>`), `GET /stats/{id}?range=live|15m|1h|24h|7d` returns the series, and `GET|PUT /stats/{id}/alerts` reads and sets the two thresholds (`cpu_high` 90 % for 10 min, `mem_high` 90 % for 5 min). Not measured, because Docker has no source for them: `cpu/iowait`, `pids/fds`, and `memory/swap` on cgroup v2; those series are absent, never zero. A firing and a resolved alert are published as `resource:alert-firing` and `resource:alert-resolved` on the message bus (properties `item`, `name`, `kind`, `situation`, `value`, `minutes`, `since`, `source`). The sampler's CPU cost has not been measured on a real Docker yet.
- **The module path is `github.com/ReCasaOS/CasaOS-AppManagement`.** The Go module and every import of it moved off `github.com/IceWhaleTech`, and the shared library is now `github.com/ReCasaOS/CasaOS-Common v0.4.22`, so logs, stack traces and `go version -m` on the shipped binary name this distribution rather than upstream; App Store catalogue and CDN icon URLs still point at IceWhale's repositories and are unchanged.
- **Loopback is not an identity** (v0.4.49). Any request from 127.0.0.1 skipped the token. Loopback is not this box's services alone: a container on the host network, or any local account, reaches the same addresses. A request skips the token only when it is one of this box's services, which come from loopback with the secret the gateway writes for this boot (`/var/run/casaos/internal.secret`, readable by root only); the dashboard always had a token. Installing a compose file is root on the host, so this mattered here most.
- **A retention for backups somebody asks for, and a copy of the catalogue** (v0.4.50). `POST /compose/{id}/backup` takes `keep`: once the backup has landed, older backups of that app at that destination beyond that number are deleted, the same arithmetic a schedule's retention uses. Absent keeps everything, as before. And the App Store's catalogue, the `store/main.zip` IceWhale builds on `CasaOS-AppStore@gh-pages`, is fetched from this distribution's nightly copy of it (`ReCasaOS/_appstore`, the same bytes) when the original stops answering, the pre-2025 `_appstore` URL an upgraded box may still carry included; the configured URL is not changed, and the original is tried first on every update (corrected in v0.4.53: v0.4.50 mapped a URL no box uses).
- **Every app at once** (v0.4.52). `GET /compose/updates` is what updating everything would do: the apps with an update on offer and, for each, the images the update would write (`check=true` asks the registries first). `POST /compose/updates` with a list of ids runs the same update the button runs, one app after another, on this box rather than from the browser, and `GET /compose/updates/run` says what became of each: updated, current with the reason, failed with the error. One run at a time.

The compose editor in an app's settings is a dashboard feature. The endpoint it saves through, `PUT /v2/app_management/compose/{id}`, is upstream; what this fork changed behind it is the `$` handling and the `.env` references above.

## Development

```sh
go generate ./...
go build ./...
go test ./route/v2/ ./service/ -count=1
```

Go 1.26.8 or later, per `go.mod`. `codegen/` is generated and not committed, so `go generate` comes first; it fetches oapi-codegen and the MessageBus spec, so the first run needs network access.

`go test ./...` also runs `./pkg/docker`, which drives a real Docker daemon and pulls images from Docker Hub. On a Windows workstation those tests fail on a goroutine-leak assertion against a `go-winio` worker; CI runs them on Linux. `TestComposeAppLifecycle` in `./service` stays skipped unless `CASAOS_INTEGRATION` is set — it needs a live CasaOS host, not just a daemon.

`DataPath` in `app-management.conf` relocates catalogues, installed compose files and stats off `/var/lib/casaos` when set; empty keeps the packaged paths. `CASAOS_GIT_APPS_DATA`, when non-empty, is the AppData root instead of `/DATA/AppData`. Neither is set on an installed box. The local process stack in CasaOS (`../CasaOS/dev/stack.sh`) sets both, and points `DOCKER_HOST` at a record-keeping Engine (`CasaOS/dev/fakedocker`) rather than `/var/run/docker.sock`. `TestComposeUpAgainstFakeDocker` in `./service` `go run`s that sibling onto a temp socket; it skips if the directory is missing. See CasaOS `dev/README.md`.

## Licence

Apache License 2.0 — see [LICENSE](LICENSE); the upstream copyright notices are kept in the source. The service is the work of IceWhale and its contributors. The Docker SDK bump and the boot recovery sweep are [alvins82](https://github.com/alvins82)'s.
