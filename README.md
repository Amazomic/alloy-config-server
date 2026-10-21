# Alloy Configuration Manager

The server serves Grafana Alloy `.alloy` configuration files selected by collector attributes. The web interface is read-only: it does not create, edit, or delete configs, selectors, or files.

## Start

```bash
docker compose up -d --build
```

- UI and HTTP/Connect API: `http://localhost:8081`
- Logs: `docker compose logs -f`
- Alloy setup example: [alloy-example.alloy](./alloy-example.alloy)

In Compose, `./configs` is mounted at `/configs` as read-only (`:ro`). The server runs as an unprivileged user. To change a config or its selector, manually edit or add a file in `./configs` on the host filesystem. Changes are detected on the next scan (1-second cache). If a new file is invalid, the server reports the error and continues serving the last working version, if available.

The config directory can be a separate Git checkout. Clone the config repository into `./configs` before starting the service. The UI shows the checked-out `HEAD` commit in the top-right corner; it does not fetch, pull, or publish changes. Update the checkout separately on the host. Keep credentials and sensitive infrastructure details out of public repositories.

## Config selection

The server selects each `.alloy` file by its selector. All matching configs are merged into one file and sent to the collector.

For an attribute selector, use the filename format `<attribute>_<value>.alloy`:

| File | Applies to |
|---|---|
| `env_prod.alloy` | Collectors with `env=prod` |
| `remote_vitacore_grafana.alloy` | Collectors with `remote=vitacore_grafana` |

The first underscore separates the attribute name from its value; underscores after that are part of the value.

A filename defines exactly one selector. For a shared config applied to all collectors, use `*.alloy`. Files with other names are shown with an error in the UI and are not served to Alloy.

Add attributes under `remotecfg.attributes` in Alloy. A selector must match all of its specified key-value pairs; additional collector attributes do not prevent a match. If the `id` key is not provided, the server adds it automatically using the collector ID.

### Collector ID substitution

In `.alloy` file contents, `{{id}}` is replaced with the collector's `id` attribute before the config is sent. The legacy `stand.190` placeholder remains supported for existing configs.

## UI and API

The UI displays collector status, attributes, and config files. If `CONFIG_DIR` contains Git metadata, the short checked-out `HEAD` commit hash appears in the top-right corner; an unborn repository is shown as having no commits. This is the local checkout revision, not a check for newer commits on GitHub. The display is read-only and does not fetch or update the repository. The free-text field above the collector table accepts attribute pairs such as `job=linux, env=prod`. Suggestions from existing attributes appear when the empty field is focused or after a comma and a space; selecting one adds it and prepares the field for another. Selected pairs are highlighted in distinct colors. Collectors are sorted first by the number of matching selected attributes; when the filter is empty or the match count is tied, collectors with the most common attribute pair appear first. Rows are not hidden. The config button displays the original file contents, including substitution placeholders. The icon in the rightmost collector column opens the final assembled config in a modal, with each merged part highlighted in a different translucent color.

Only GET routes are available for the UI/API:

| Route | Purpose |
|---|---|
| `/` | Web interface |
| `/healthz` | Health check |
| `/api/state` | Collector state, configs, and events |
| `/api/config/{name}` | Contents of `name.alloy` |
| `/api/collector/{id}/config` | Latest assembled config for the collector |

Collector registration records are updated through Alloy RPC and kept in memory; these operations cannot write to config files.

## Security and access boundaries

- The application has no UI/API operations for writing to the config directory: the server reads `.alloy` files but does not modify them. Config filenames, selectors, and contents can only be changed by modifying files on the filesystem.
- Compose also mounts `./configs` as read-only. This prevents writes from inside the container; host-side changes remain possible and are the intended way to update configs.
- The UI and API have **no authentication** and use HTTP without TLS. Anyone who can connect to the port can view configs and state and access the Alloy RPC. Do not expose the port to untrusted networks or the internet; restrict access with a firewall/private network or use a trusted reverse proxy with TLS and authentication.
- Alloy RPC accepts collector IDs and attributes supplied by the collector to select configs. Without authentication, a network-accessible client can impersonate a collector and request configs matching its chosen attributes. It cannot write or rename files, but this should be considered when configuring network access.
- Any system or user with write access to `./configs` on the host can change configs and selectors. The read-only mount prevents writes from the container, not from the host.

## Server settings

| Variable | Default | Purpose |
|---|---|---|
| `CONFIG_DIR` | `/configs` | Config file directory |
| `LISTEN_ADDR` | `:8080` | Address and port inside the container |
| `LOG_LEVEL` | `info` | Log level |
| `LOG_FORMAT` | `text` | Log format; `json` enables JSON |
| `LOG_NOT_MODIFIED` | `true` | Log polls with no config changes |

Collector state and events are held in memory and cleared when the server restarts. Config files in `CONFIG_DIR` are the source of truth.
