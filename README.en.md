# Alloy Configuration Hub

> Uses Grafana Alloy’s [`remotecfg`](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/) block and implements the [Alloy Remote Config API](https://github.com/grafana/alloy-remote-config).

English · [Русский](README.md)

A read-only hub that selects configurations by collector attributes, displays collector status, and supports relays with persistent offline cache.

![Alloy Configuration Hub dashboard](docs/images/hub-dashboard.jpg)

*Demonstration data: collectors, configurations, events, and the current commit hash.*

## Your configuration repository

Keep `.alloy` files separate from the application. Clone your own repository, copy `.env.example` to `.env`, and set `CONFIG_SOURCE_DIR`:

```bash
git clone <YOUR_CONFIGURATION_REPOSITORY_URL> ../alloy-configs
cp .env.example .env
```

```dotenv
CONFIG_SOURCE_DIR=../alloy-configs
```

An ordinary directory without Git also works. It must exist before startup; the hub mounts it read-only and reads only `.alloy` files in its root.

When `.git` is present, the header displays a short `HEAD` hash linked to the commit in `origin`. Without a remote the hash is plain text; without Git it is hidden. For submodules/worktrees, optionally set `CONFIG_GIT_DIR` to the path returned by `git -C ../alloy-configs rev-parse --absolute-git-dir`.

Files refresh through a one-second cache, but the hub never runs Git operations. Update the working copy yourself: `git -C ../alloy-configs pull --ff-only`.

## Configuration selection

- `env_prod.alloy` matches `env=prod`; the first underscore separates attribute and value. Legacy `env=prod.alloy` names also work.
- A literal `*.alloy` filename applies to every collector and requires a filesystem allowing `*`.
- Matching files merge inside `declare "node_stack"`: shared files first, then filename order. Component names must not conflict.
- `{{name}}` expands to the `name` attribute, falling back to collector `id`. Source filenames are never injected into the final text.

The hub checks names and readability; Alloy checks syntax on application. Unreadable files retain their last readable version when available.

## Run: one Compose file, three profiles

Profiles are explicit; bare `docker compose up` selects no services.

| Deployment | Command |
|---|---|
| Hub | `docker compose --profile hub up -d --build` |
| Hub + Alloy | `docker compose --profile hub --profile alloy up -d --build` |
| Relay | `docker compose --profile relay up -d --build` |
| Relay + Alloy | `docker compose --profile relay --profile alloy up -d --build` |
| Everything | `docker compose --profile hub --profile relay --profile alloy up -d --build` |

Set `UPSTREAM_URL` for a relay: an external hub address, or `http://alloy_configuration_hub:8080` when the hub runs alongside it. Set `ALLOY_CONFIG_URL=http://alloy_config_relay:8080` to route Alloy through the relay.

The `alloy` profile needs a bootstrap file at `ALLOY_CONFIG_FILE`, for example:

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

cAdvisor host metrics require a **Linux Docker host**: Alloy runs privileged with host resource mounts. Docker Desktop exposes Linux VM metrics. The hub and relay run unprivileged.

## Key settings

See [.env.example](.env.example); in PowerShell use `Copy-Item .env.example .env`.

| Variable | Default / purpose |
|---|---|
| `CONFIG_SOURCE_DIR`, `CONFIG_GIT_DIR` | `./configs`; separate Git metadata is optional |
| `HUB_PORT`, `RELAY_PORT`, `ALLOY_PORT` | `8081`, `8082`, `12345`; hub and relay use `8080` internally |
| `HUB_BIND_ADDRESS`, `RELAY_BIND_ADDRESS`, `ALLOY_BIND_ADDRESS` | `0.0.0.0`; use `127.0.0.1` for local access |
| `UPSTREAM_URL`, `UPSTREAM_BEARER_TOKEN` | Hub address and optional relay-owned token |
| `RELAY_NAME`, `RELAY_TIMEOUT`, `RELAY_CACHE_MAX_AGE` | `config-relay`, `5s`, `0s` (indefinite); e.g. `24h` limits cache age |
| `ALLOY_CONFIG_FILE`, `ALLOY_CONFIG_URL` | `./configs/collector/config.alloy`, `http://alloy_configuration_hub:8080` |
| `ALLOY_ID`, `ALLOY_ENV`, `ALLOY_REMOTE` | `alloy-local`, `prod`, `local` |

Stopping preserves `alloy_data` and `relay_cache`; `down -v` deletes them. `.env`, local configs, and secrets are ignored by Git and excluded from the Docker image.

## Dashboard and relay

The header shows collectors and relays active in the last five minutes, configuration count, and commit hash. The dashboard refreshes every three seconds; source blocks are highlighted and Copy returns clean configuration text.

`Sent` awaits acknowledgement; `Received` confirms a hash or application; `Error` means Alloy reported failure. `Last synced` records version acknowledgement, but matching hashes alone do not imply `APPLIED`. In-memory state resets on restart.

Main GET APIs: `/api/state`, `/api/config/{name}`, `/api/collector/{id}/config` (also `?format=json`), and `/api/collector/{id}/effective-config`. `/healthz` checks process liveness; `/api/relay` is available in relay mode.

Relays forward configs and reports, not telemetry. Transient upstream failures use cached content subject to `RELAY_CACHE_MAX_AGE`; access errors never fall back to cache. Each poll retries upstream. Disk cache survives restart; pending in-memory reports do not.

## Security and development

- No built-in authentication or TLS. Restrict access through network rules or an authenticated TLS reverse proxy. Client IDs and attributes are not authenticated identities.
- Keep secrets out of `.alloy`: APIs and cache contain the full text. Load secrets on Alloy hosts using `local.file` with `is_secret = true`.
- Development requires Go 1.26+: `go test ./...`, `go test -race ./...`, `go vet ./...`, `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.

---

## 🚀 Quick look

> [!TIP]
> **Start the hub without a repository or collectors.** You need Docker with Compose and a terminal in the project directory. Tables will initially be empty.

```bash
mkdir -p ../alloy-configs-demo
CONFIG_SOURCE_DIR=../alloy-configs-demo HUB_BIND_ADDRESS=127.0.0.1 docker compose --profile hub up -d --build
```

<details>
<summary><strong>PowerShell</strong></summary>

```powershell
New-Item -ItemType Directory -Path ../alloy-configs-demo -Force | Out-Null
$env:CONFIG_SOURCE_DIR = "../alloy-configs-demo"
$env:HUB_BIND_ADDRESS = "127.0.0.1"
docker compose --profile hub up -d --build
```

</details>

**Open → [http://localhost:8081](http://localhost:8081)**

Logs: `docker compose --profile hub logs -f alloy_configuration_hub`

Stop: `docker compose --profile hub down`
