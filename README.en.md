# Alloy Configuration Hub

> Uses Grafana Alloy’s [`remotecfg`](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/) block and implements the [Alloy Remote Config API](https://github.com/grafana/alloy-remote-config): collectors poll for and apply remote configuration.

English · [Русский](README.md)

A read-only configuration hub for Grafana Alloy, with collector status, source-block highlighting, and relays with persistent offline cache.

![Alloy Configuration Hub dashboard](docs/images/hub-dashboard.jpg)

*Collectors, configurations, events, and the current commit hash. The screenshot uses demonstration data.*

## Connect your configuration repository

Keep `.alloy` files in your own separate Git repository and connect its working copy to the hub yourself. No specific repository is required. Set `CONFIG_SOURCE_DIR` to any existing configuration directory; you may use a free `configs` directory or store configs beside the application checkout.

For example:

```bash
git clone <YOUR_CONFIGURATION_REPOSITORY_URL> ../alloy-configs
```

Copy `.env.example` to `.env` and set:

```dotenv
CONFIG_SOURCE_DIR=../alloy-configs
```

The hub reads `.alloy` files directly in that directory; subdirectories are not merged. Mounts are read-only. Git is optional: an ordinary directory also works.

When `.git` is present, the hub reads `HEAD` and shows **only a short commit hash** in the header. The hash links to the commit in the repository identified by `remote.origin.url`, converting SSH remotes to web URLs. Without a suitable remote the hash is plain text; without Git metadata the badge is hidden. The hub does not execute Git, contact GitHub, fetch, or pull.

Both `.git` directories and `gitdir` pointer files are supported, including detached HEAD, packed refs, and worktrees. Submodule/worktree metadata may reside outside the configuration directory. In Docker, expose that metadata by setting `CONFIG_GIT_DIR` to the directory returned by:

```bash
git -C ../alloy-configs rev-parse --absolute-git-dir
```

For an ordinary clone, leave `CONFIG_GIT_DIR` empty: `.git` is read from `CONFIG_SOURCE_DIR` automatically.

The optional `alloy` profile also needs a collector bootstrap. Point `ALLOY_CONFIG_FILE` to it, e.g. `../alloy-configs/collector/config.alloy`:

```alloy
remotecfg {
  url            = sys.env("ALLOY_CONFIG_URL")
  id             = constants.hostname
  poll_frequency = "30s"

  attributes = {
    name   = constants.hostname,
    env    = sys.env("ALLOY_ENV"),
    remote = sys.env("ALLOY_REMOTE"),
  }
}
```

Local root-level `.alloy` files, `.env`, secrets, and `data` are ignored by Git. The Docker context admits only Go sources, module files, and web assets, excluding configs and Git metadata.

## One Compose file, three profiles

All services live in `docker-compose.yml`. Profiles are explicit; bare `docker compose up` selects no services.

| Deployment | Command |
|---|---|
| Hub only | `docker compose --profile hub up -d --build` |
| Hub and Alloy | `docker compose --profile hub --profile alloy up -d --build` |
| Relay only | `docker compose --profile relay up -d --build` |
| Relay and Alloy | `docker compose --profile relay --profile alloy up -d --build` |
| All components | `docker compose --profile hub --profile relay --profile alloy up -d --build` |

Set `UPSTREAM_URL` before starting a relay. Set `ALLOY_CONFIG_URL=http://alloy_config_relay:8080` for Alloy to use that relay; otherwise Alloy connects directly to the hub. An Alloy-only deployment needs a reachable external hub or relay URL.

Optionally copy `.env.example` to `.env` (`cp .env.example .env`, or `Copy-Item .env.example .env` in PowerShell). Its upstream address assumes a local hub; change it for a standalone relay.

Complete local relay chain in Bash:

```bash
UPSTREAM_URL=http://alloy_configuration_hub:8080 \
ALLOY_CONFIG_URL=http://alloy_config_relay:8080 \
docker compose --profile hub --profile relay --profile alloy up -d --build
```

Standalone relay in PowerShell:

```powershell
$env:UPSTREAM_URL = "http://MAIN_HUB:8081"
docker compose --profile relay up -d --build
```

Default host ports: hub `8081`, relay `8082`, Alloy `12345`. Hub and relay listen on `8080` inside Compose. `host.docker.internal` means the Docker host, not an arbitrary upstream machine.

```bash
docker compose --profile hub --profile relay --profile alloy config --quiet
docker compose --profile hub logs -f alloy_configuration_hub
docker compose --profile relay logs -f alloy_config_relay
docker compose --profile hub --profile relay --profile alloy down
```

Stopping preserves `alloy_data` and `relay_cache`; `down -v` deletes them. Avoid `--remove-orphans` without checking other services in the project. Existing service names and published ports are preserved from the separate Compose files; stop the older `alloy_conf_manager` service before starting `alloy_configuration_hub`.

The `alloy` profile requires a **Linux Docker host** for cAdvisor host metrics. It uses privileged mode, host PID/cgroup, and host resource mounts. Docker Desktop exposes its Linux VM rather than Windows/macOS host metrics. The hub and relay run unprivileged.

## Settings

See [.env.example](.env.example).

| Variable | Default / purpose |
|---|---|
| `HUB_PORT`, `HUB_BIND_ADDRESS` | `8081`, `0.0.0.0` |
| `CONFIG_SOURCE_DIR` | `./configs`, your configuration directory |
| `CONFIG_GIT_DIR` | Empty: use `.git` in `CONFIG_SOURCE_DIR`; optionally an explicit Git metadata directory |
| `RELAY_PORT`, `RELAY_BIND_ADDRESS` | `8082`, `0.0.0.0` |
| `UPSTREAM_URL` | Required only when a relay is running |
| `RELAY_NAME` | `config-relay`, identity in hub logs |
| `RELAY_TIMEOUT` | `5s`, upstream timeout |
| `RELAY_CACHE_MAX_AGE` | `0s` permits indefinite offline cache use; e.g. `24h` limits it |
| `UPSTREAM_BEARER_TOKEN` | Optional relay-owned upstream token |
| `ALLOY_ID` | `alloy-local`, container hostname and collector ID |
| `ALLOY_PORT`, `ALLOY_BIND_ADDRESS` | `12345`, `0.0.0.0` |
| `ALLOY_CONFIG_FILE` | `./configs/collector/config.alloy` |
| `ALLOY_CONFIG_URL` | `http://alloy_configuration_hub:8080` |
| `ALLOY_ENV`, `ALLOY_REMOTE` | `prod`, `local`, collector attributes |

Direct binary settings: `APP_MODE=server|relay`, `LISTEN_ADDR=:8080`, `CONFIG_DIR=/configs`, `CONFIG_REVISION_DIR` (defaults to `CONFIG_DIR`), `LOG_LEVEL=info`, `LOG_FORMAT=text|json`, `LOG_NOT_MODIFIED=true`. `RELAY_CACHE_DIR=/var/lib/alloy-config-server/relay-cache` is retained for compatibility and must be writable by UID 65532.

Configuration and metadata mounts are read-only. Missing directories cause startup errors rather than being silently created. An existing configuration directory without `.git` still works.

## Selection and updates

`<attribute>_<value>.alloy` matches a collector attribute, e.g. `env_prod.alloy` matches `env=prod`. The first underscore separates key and value. Legacy `<attribute>=<value>.alloy` names also work. A literal `*.alloy` filename matches all collectors; creating that filename requires a filesystem allowing `*`, such as Linux.

Matching files are merged into `declare "node_stack"`, instantiated as `node_stack "default"`. Shared files come first, then filename order. Avoid selecting alternative templates with conflicting component names.

The only server placeholder is `{{name}}`: the `name` attribute, falling back to `id` (automatically populated from the collector ID when absent). Values are escaped for Alloy strings. Source filenames are separate display metadata and never injected as comments. The hash covers the final rendered text.

The hub checks names and readability; Alloy checks syntax when applying the result. A one-second cache refreshes file contents. Unreadable files retain their last readable version when available; invalid names are excluded.

Update configurations in your own repository. For an ordinary working copy:

```bash
git -C ../alloy-configs pull --ff-only
```

Commit and publish your edits in the configuration repository first. If you choose a submodule, separately update its pointer in the containing repository. The hub rereads available files but does not fetch, pull, or write them.

## Dashboard and API

Header counts show active collectors, configuration files, and connected relays. Activity means a request in the last five minutes, excluding explicitly disconnected collectors. Relays are deduplicated by name and peer address; only the nearest relay in a chain is visible.

Polling runs every three seconds and pauses in hidden tabs, while selecting text, or while a configuration dialog is open. Source blocks are colored by selector, with source names in hover titles. Copy returns clean configuration text. The header shows the configuration directory’s current commit hash, linked to its remote when available.

`Sent` means a configuration was prepared for delivery. `Received` means its hash or application was confirmed; the tooltip distinguishes them. `Error` means Alloy reported failure for that version. `Last synced` records a matching hash acknowledgement, `APPLIED`, or a fresh exactly matching effective config; `FAILED` and `APPLYING` are not successful synchronization. Receipt and `applied_hash` are tracked separately. State is in memory and resets on restart.

| GET route | Purpose |
|---|---|
| `/` | Hub dashboard; JSON status in relay mode |
| `/healthz` | Process liveness, independent of upstream |
| `/api/state` | Collectors, configs, events, counts |
| `/api/config/{name}` | Original `.alloy` file |
| `/api/collector/{id}/config` | Last assembled configuration; `?format=json` adds block metadata |
| `/api/collector/{id}/effective-config` | Last Alloy-reported configuration; 404 until a report arrives |
| `/api/relay` | Relay status, only in relay mode |

Standard Alloy Remote Config RPCs (`GetConfig`, `RegisterCollector`, `UnregisterCollector`) share the same listener and cannot modify files.

## Relay recovery

Relays forward configurations, identities, attributes, and reports, not telemetry. Alloy must still reach its metrics and logs destinations.

Cache entries are isolated by upstream, token, ID, and attributes, with hashed filenames and integrity digests. Transient upstream failures use a matching cache subject to `RELAY_CACHE_MAX_AGE`. Every poll retries upstream. `X-Config-Source` is `upstream` or `cache`.

`Unauthenticated`, `PermissionDenied`, and `NotFound` invalidate cached access rather than falling back. Redirects are refused. Incoming authorization is not forwarded; `UPSTREAM_BEARER_TOKEN` supplies relay-owned credentials. `X-Alloy-Relay-Name` and `X-Alloy-Relay-Hops` produce `relay_config` events; chains are limited to eight hops.

Pending status/effective-config reports and deferred registration are retained in memory and retried after recovery. Unregistration is never queued. Restart loses pending reports but retains disk cache. A cached configuration is the last received version, not necessarily the last successfully applied version.

## Security

- No built-in incoming authentication or TLS. Ports bind all interfaces by default. Restrict access with network/firewall rules or an authenticated TLS reverse proxy; set `*_BIND_ADDRESS=127.0.0.1` for a local proxy.
- Client IDs, attributes, and relay headers are not authenticated identities. Without external access control clients can request other collectors' configs; in-memory identity records have no global count limit.
- RPC requests are limited to 16 MiB and HTTP headers to 64 KiB. These limits do not replace access control.
- Keep secrets out of `.alloy` and attributes. Use local secret files with `local.file` and `is_secret = true`. Inline secrets are visible in APIs, dialogs, and relay cache.
- Private Git access does not protect the running HTTP API. Ignore rules do not remove secrets from previous commits. Remote credentials and query parameters are never included in the commit link.

## Development

Go 1.26 or newer is required:

```bash
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
docker build -t alloy-configuration-hub .
```

Module versions and checksums are committed. Docker builds do not run `go mod tidy`.

---

## 🚀 Quick look

> [!TIP]
> **Try the hub now.** You need Docker with Compose and a terminal in the project directory. An empty configuration folder is enough for a first look; connect your Git repository and Alloy later using the instructions above.

**Bash · two commands:**

```bash
mkdir -p ../alloy-configs-demo
CONFIG_SOURCE_DIR=../alloy-configs-demo HUB_BIND_ADDRESS=127.0.0.1 docker compose --profile hub up -d --build
```

<details>
<summary><strong>PowerShell commands</strong></summary>

```powershell
New-Item -ItemType Directory -Path ../alloy-configs-demo -Force | Out-Null
$env:CONFIG_SOURCE_DIR = "../alloy-configs-demo"
$env:HUB_BIND_ADDRESS = "127.0.0.1"
docker compose --profile hub up -d --build
```

</details>

### Open → [http://localhost:8081](http://localhost:8081)

Tables will initially be empty. Point the hub to your configuration directory and connect collectors when ready.

Logs: `docker compose --profile hub logs -f alloy_configuration_hub`

Stop: `docker compose --profile hub down`
