# alloy-config-server

Минимальный remote config сервер для Grafana Alloy (`remotecfg`). Один контейнер, один порт (RPC + UI), без авторизации и редактирования.

```bash
docker compose up -d --build
# UI:  http://localhost:8080
# Alloy: remotecfg { url = "http://<host>:8080" }   (см. alloy-example.alloy)
docker compose logs -f
```

## Конфиги

```
configs/
  10-base/        config.alloy, labels.yaml   # labels: {}  → всем Alloy
  20-web-prod/    nginx.alloy,  labels.yaml   # env=prod, role∈{web,api}
  30-db-prod/     config.alloy, labels.yaml   # env=*, role=db
```

Одна папка = один pipeline (все `*.alloy` внутри склеиваются). `labels.yaml`:

```yaml
description: необязательно
labels:
  env: prod            # точное значение
  role: [web, api]     # любое из списка
  region: "*"          # лейбл должен быть, значение любое
```

- Alloy получает **все** pipelines, у которых каждый лейбл совпал с его `attributes` (AND по ключам, OR по значениям), склеенные в порядке имён папок. Нумеруйте префиксом (`10-`, `20-`).
- Нет `labels.yaml` → pipeline отключён. `labels: {}` → подходит всем.
- Виртуальный лейбл `collector.id` = `id` из `remotecfg` — для привязки к конкретному хосту.
- Файлы перечитываются на каждый запрос (кэш 1 с), перезапуск не нужен. Если файлы стали битыми, отдаётся последняя рабочая версия + предупреждение в логе и UI.
- Компоненты разных pipelines не должны иметь одинаковых лейблов (`prometheus.scrape "node"` дважды = ошибка в Alloy).

## Логи

Каждый `GetConfig` — одна строка: `id`, `addr`, `attrs`, выбранные `configs`, `client_hash`/`server_hash`, `result`:
`initial` (первый конфиг) · `update_sent` (отдали новый) · `synced` (Alloy вернулся с новым хэшем = применил) · `not_modified`.
`failed` статус от Alloy — WARN с текстом ошибки. `LOG_NOT_MODIFIED=false` убирает шум от обычных опросов; `LOG_FORMAT=json` для Loki.

Состояние Alloy (UI, события) хранится в памяти и сбрасывается при рестарте.
