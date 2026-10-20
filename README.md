# alloy-config-server

Минимальный remote config сервер для Grafana Alloy (`remotecfg`). Один контейнер, один порт (RPC + UI), без авторизации.

```bash
docker compose up -d --build
# UI:  http://localhost:8081
# Alloy: remotecfg { url = "http://<host>:8081" }   (см. alloy-example.alloy)
docker compose logs -f
```

`./configs` — конфиги `*.alloy` (только чтение). Никаких БД и привязок: **имя файла и есть условие выдачи**.

## Как это работает

Атрибуты из `remotecfg.attributes` (плюс виртуальный `id` = `id` collector'а) сопоставляются с именами файлов. Разделитель — **нижнее подчёркивание**, никаких «=» в именах: файл `env_prod.toml`/`env_prod.alloy` читается как лейбл `env` со значением `prod`. У каждого файла ровно одна такая метка.

| Имя файла | Кто получит |
|---|---|
| `env_prod.alloy` | Alloy, у которого в attributes есть `env = "prod"` (лейбл `env=prod`) |
| `os_linux.alloy` | Alloy с атрибутом `os = "linux"` |
| `id_node-1.alloy` | только Alloy с `id = "node-1"` (персональный конфиг) |
| `*.alloy` | всем без исключения (базовый конфиг, склеивается первым) |

- Чтобы отдать конфигурационный файл какому-то Alloy — добавь нужный атрибут ему в `remotecfg.attributes`; имя файла уже содержит метку.
- Формат имени: `<ключ>_<значение>.alloy`. Ключ — всё до первого `_`, значение — всё после (может содержать дефисы и точки, но не `_`). Файлы без `_` (кроме `*`) игнорируются с предупреждением в логе.
- Alloy получает **все** совпавшие файлы, склеенные в один конфиг (порядок — по именам файлов, `*` первым).
- Файлы перечитываются на запрос (кэш 1 с). Битые → последняя рабочая версия + предупреждение.

Пример:

```
configs/
  *.alloy               // база для всех
  env_prod.alloy        // env="prod"
  os_linux.alloy        // linux-хосты
  role_web.alloy        // веб-ноды
  id_canary-01.alloy    // персональный конфиг одного collector'а
```

API:

```
GET    /api/state                 // конфиги, collectors, события
GET    /api/config/{name}         // текст конфига (name = "env_prod")
GET    /api/match?env=prod&role=web   // что получит Alloy с такими атрибутами
```

## Логи

Каждый `GetConfig`: `id`, `attrs`, выбранные `configs`, хэши, `result` (`initial` · `update_sent` · `synced` · `not_modified`).
Состояние Alloy в UI — в памяти. Всё состояние конфигов — в именах и содержимом файлов в `CONFIG_DIR`.
