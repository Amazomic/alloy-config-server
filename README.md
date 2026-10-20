# alloy-config-server

Минимальный remote config сервер для Grafana Alloy (`remotecfg`). Один контейнер, один порт (RPC + UI), без авторизации.

```bash
docker compose up -d --build
# UI:  http://localhost:8081
# Alloy: remotecfg { url = "http://<host>:8081" }   (см. alloy-example.alloy)
docker compose logs -f
```

`./configs` — шаблоны `*.alloy` (только чтение). `./data` — привязки лейблов (`bindings.json`), чтобы не терялись после рестарта. Логирование по умолчанию: info / text / все GetConfig.

## Шаблоны

Одна папка — плоский список файлов:

```
configs/
  logs.alloy
  metrics.alloy
  docker.alloy
```

Лейблы **не** лежат рядом с файлами. Связь «шаблон → лейблы» хранится на сервере в `DATA_DIR/bindings.json` (volume `./data`), чтобы переживать рестарт. В UI у каждого шаблона есть кнопка «лейблы».

- Без привязки шаблон никому не отдаётся.
- `labels: {}` (пустой набор) — подходит всем Alloy.
- `env: prod`, `role: [web, api]`, `region: "*"` — точное значение / любое из списка / лейбл должен быть.
- Alloy получает **все** шаблоны, у которых каждый лейбл совпал с его `attributes` (AND по ключам, OR по значениям), склеенные в порядке имён файлов.
- Виртуальный лейбл `collector.id` = `id` из `remotecfg` — для привязки к конкретному хосту.
- Файлы перечитываются на каждый запрос (кэш 1 с). Битые файлы → последняя рабочая версия + предупреждение.
- Компоненты разных шаблонов не должны иметь одинаковых лейблов (`prometheus.scrape "node"` дважды = ошибка в Alloy).

API:

```
PUT  /api/config/{name}/labels   {"labels":{"env":"prod","role":["web","api"]},"description":"..."}
DELETE /api/config/{name}/labels
GET  /api/match?env=prod&role=web
```

## Логи

Каждый `GetConfig` — одна строка: `id`, `addr`, `attrs`, выбранные `configs`, `client_hash`/`server_hash`, `result`:
`initial` · `update_sent` · `synced` · `not_modified`.
`failed` от Alloy — WARN. `LOG_NOT_MODIFIED=false` убирает шум; `LOG_FORMAT=json` для Loki.

Состояние Alloy (UI, события) — в памяти, сбрасывается при рестарте. Привязки лейблов — на диске в `DATA_DIR`.
