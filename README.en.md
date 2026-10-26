# Alloy Configuration Hub

English · [Русский](README.md)

The hub serves Grafana Alloy configurations selected by collector attributes. Alloy fetches them through `remotecfg`; the dashboard shows collectors, configs, and events.

```text
configs/*.alloy → Hub ← Alloy
                      ← Relay ← Alloy (optional)
```

A relay forwards configurations and reports between Alloy and the hub, caching configs in case the hub becomes unavailable. Alloy sends metrics, logs, and traces directly to the configured storage systems.

![Alloy Configuration Hub dashboard](docs/images/hub-dashboard.jpg)

## Start the hub

You need Docker and Compose. From the project directory:

```bash
git submodule update --init --recursive
docker compose --profile hub up -d --build
```

Dashboard: [http://localhost:8081](http://localhost:8081).

Configurations live in `configs/`, the included template repository. Adapt the `.alloy` files to your infrastructure, including telemetry destinations. The hub automatically rereads files without a restart. Pulling updates from Git is a separate step.

## Connect Alloy

Add this to your local `config.alloy`:

```alloy
remotecfg {
  url            = "http://hub-server:8081"
  id             = constants.hostname
  poll_frequency = "30s"

  attributes = {
    name   = constants.hostname,
    env    = "prod",
    remote = "local",
  }
}
```

Replace `hub-server` with the hub server's IP or hostname. If Alloy runs on the same host outside a container, you can use `http://localhost:8081`. Attributes are set directly in the config; no environment variables are needed.

Start or restart Alloy with this file. The collector appears in the dashboard after its first request.

## How configs are selected

The hub reads `.alloy` files from the root of `configs/`. Each filename specifies an attribute and value:

| File | Alloy attribute |
|---|---|
| `env_prod.alloy` | `env = "prod"` |
| `remote_local.alloy` | `remote = "local"` |
| `job_grafana.alloy` | `job = "grafana"` |

All matching files merge inside `declare "node_stack"` and run through `node_stack "default"`. The `{{name}}` placeholder expands to the `name` attribute, falling back to the collector ID.

Select compatible templates: `env_prod.alloy` and `job_grafana.alloy` have overlapping component names and represent alternative collection setups. Alloy validates the resulting configuration syntax.

## Relay — optional

On the relay server, create a `.env` file with the hub address:

```dotenv
UPSTREAM_URL=http://hub-server:8081
```

```bash
docker compose --profile relay up -d --build
```

Point collectors' `remotecfg` at the relay: `url = "http://relay-server:8082"`. During temporary hub outages, the relay serves the last cached config. The cache survives restarts.

## Additional details

- To run a test Alloy in the same Compose: `docker compose --profile hub --profile alloy up -d --build`. It uses `configs/collector/config.alloy`; settings are in [.env.example](.env.example). Host metrics collection through cAdvisor targets Linux.
- The default config directory is `./configs`. Set `CONFIG_SOURCE_DIR` in `.env` if you need another directory. Port and cache settings are also listed in [.env.example](.env.example).
- The dashboard supports light and dark themes, config viewing, and copying. `Sent` awaits acknowledgement, `Received` confirms receipt, and `Error` indicates an application failure. Disconnected collectors remain inactive for five minutes before disappearing; without an explicit disconnect, inactivity starts after five minutes without requests.
- There is no built-in authentication or TLS: use a private network or a secured reverse proxy. Keep secrets on Alloy hosts rather than in distributed configs.
- Development: Go 1.26+, `go test ./...`, `go test -race ./...`, `go vet ./...`.
