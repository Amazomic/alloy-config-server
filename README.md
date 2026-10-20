# alloy-config-server

Минимальный remote config сервер для Grafana Alloy (`remotecfg`). Один контейнер, один порт (RPC + UI), без авторизации.

```bash
docker compose up -d --build
# UI:  http://localhost:8081
# Alloy: remotecfg { url = "http://<host>:8081" }   (см. alloy-example.alloy)
docker compose logs -f
```

`./configs` — шаблоны `*.alloy` (только чтение). `./data` — привязки лейблов (`bindings.json`), чтобы не терялись после рестарта.

## Шаблоны

```
configs/
  logs.alloy
  metrics.alloy
  docker.alloy
```

На каждый шаблон вешается **один** лейбл (`key=value`). Связь хранится в `DATA_DIR/bindings.json`, не рядом с файлами.

- Без лейбла шаблон никому не отдаётся.
- Alloy получает все шаблоны, чей лейбл есть в его `attributes`, склеенные в один конфиг (порядок — по именам файлов).
- `value: "*"` — подходит любое значение этого ключа.
- Виртуальный атрибут `collector.id` = `id` из `remotecfg`.
- Файлы перечитываются на запрос (кэш 1 с). Битые → последняя рабочая версия + предупреждение.

API:

```
PUT    /api/config/{name}/label   {"key":"collector.os","value":"linux","description":"..."}
DELETE /api/config/{name}/label
GET    /api/match?collector.os=linux
```

## Логи

Каждый `GetConfig`: `id`, `attrs`, выбранные `templates`, хэши, `result` (`initial` · `update_sent` · `synced` · `not_modified`).
Состояние Alloy в UI — в памяти. Привязки лейблов — на диске в `DATA_DIR`.
