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

Атрибуты из `remotecfg.attributes` (плюс виртуальный `id` = `id` collector'а) сопоставляются с именами файлов:

| Имя файла | Кто получит |
|---|---|
| `os=linux.alloy` | Alloy, у которого в attributes есть `os = "linux"` |
| `env=*.alloy` | любой Alloy, у которого вообще есть атрибут `env` |
| `id=node-1.alloy` | только Alloy с `id = "node-1"` (персональный конфиг) |
| `*.alloy` | всем без исключения (базовый конфиг, склеивается первым) |

- Чтобы отдать конфигурационный файл какому-то Alloy — добавь нужный атрибут ему в `remotecfg.attributes`; имя файла уже содержит условие.
- Формат имени: `<ключ>=<значение>.alloy`, где ключ — имя атрибута (без префиксов). Файлы без `=` (кроме `*`) игнорируются с предупреждением в логе.
- Alloy получает **все** совпавшие файлы, склеенные в один конфиг (порядок — по именам файлов, `*` первым).
- Файлы перечитываются на запрос (кэш 1 с). Битые → последняя рабочая версия + предупреждение.

Пример:

```
configs/
  *.alloy               // база для всех
  os=linux.alloy        // linux-хосты
  role=web.alloy        // веб-ноды
  id=canary-01.alloy    // персональный конфиг одного collector'а
```

API:

```
GET    /api/state                 // конфиги, collectors, события
GET    /api/config/{name}         // текст конфига (name = "os=linux")
GET    /api/match?os=linux&role=web   // что получит Alloy с такими атрибутами
```

## Логи

Каждый `GetConfig`: `id`, `attrs`, выбранные `configs`, хэши, `result` (`initial` · `update_sent` · `synced` · `not_modified`).
Состояние Alloy в UI — в памяти. Всё состояние конфигов — в именах и содержимом файлов в `CONFIG_DIR`.
