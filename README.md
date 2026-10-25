# Alloy Configuration Hub

> Использует [`remotecfg`](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/) Grafana Alloy и реализует [Alloy Remote Config API](https://github.com/grafana/alloy-remote-config).

[English](README.en.md) · Русский

Hub выдаёт конфигурации по атрибутам коллекторов, показывает их состояние и поддерживает ретрансляторы с офлайн-кешем. Интерфейс доступен только для чтения.

![Интерфейс Alloy Configuration Hub](docs/images/hub-dashboard.jpg)

*Демонстрационные данные: коллекторы, конфигурации, события и хеш коммита.*

## Свой репозиторий конфигураций

Храните `.alloy`-файлы отдельно от приложения. Клонируйте собственный репозиторий, скопируйте `.env.example` в `.env` и задайте `CONFIG_SOURCE_DIR`:

```bash
git clone <URL_ВАШЕГО_РЕПОЗИТОРИЯ> ../alloy-configs
cp .env.example .env
```

```dotenv
CONFIG_SOURCE_DIR=../alloy-configs
```

Подойдёт и обычная папка без Git. Она должна существовать до запуска; Hub читает только `.alloy`-файлы в её корне и монтирует её только для чтения.

При наличии `.git` в шапке появляется короткий хеш `HEAD` со ссылкой на коммит из `origin`. Без remote хеш отображается без ссылки, без Git — скрыт. Для submodule/worktree при необходимости задайте `CONFIG_GIT_DIR`: путь можно получить через `git -C ../alloy-configs rev-parse --absolute-git-dir`.

Hub перечитывает файлы с кешем в одну секунду, но не выполняет Git-операции. Обновляйте рабочую копию самостоятельно: `git -C ../alloy-configs pull --ff-only`.

## Выбор конфигураций

- `env_prod.alloy` применяется при `env=prod`; первое подчёркивание разделяет атрибут и значение. Старый формат `env=prod.alloy` тоже поддерживается.
- Буквальное имя `*.alloy` означает общий конфиг; создать его можно на файловой системе, допускающей `*`.
- Совпавшие файлы объединяются внутри `declare "node_stack"`: общие первыми, затем по имени. Имена компонентов не должны конфликтовать.
- `{{name}}` заменяется атрибутом `name`, затем `id` коллектора. Имена файлов в итоговый текст не добавляются.

Hub проверяет имена и читаемость файлов; синтаксис проверяет Alloy при применении. При ошибке чтения сохраняется последняя читаемая версия, если она есть.

## Запуск: один Compose, три профиля

Профили включаются явно; без них `docker compose up` не выбирает сервисы.

| Вариант | Команда |
|---|---|
| Hub | `docker compose --profile hub up -d --build` |
| Hub + Alloy | `docker compose --profile hub --profile alloy up -d --build` |
| Ретранслятор | `docker compose --profile relay up -d --build` |
| Ретранслятор + Alloy | `docker compose --profile relay --profile alloy up -d --build` |
| Всё | `docker compose --profile hub --profile relay --profile alloy up -d --build` |

Для ретранслятора задайте `UPSTREAM_URL`: адрес внешнего Hub либо `http://alloy_configuration_hub:8080`, если Hub запущен рядом. Чтобы Alloy обращался через ретранслятор, задайте `ALLOY_CONFIG_URL=http://alloy_config_relay:8080`.

Профиль `alloy` требует bootstrap-файл в `ALLOY_CONFIG_FILE`, например:

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

Для cAdvisor нужен **Linux Docker host**: Alloy работает с `privileged` и доступом к системным каталогам. В Docker Desktop собираются метрики Linux VM. Hub и ретранслятор непривилегированные.

## Основные настройки

Полный пример — [.env.example](.env.example); PowerShell: `Copy-Item .env.example .env`.

| Переменная | По умолчанию / назначение |
|---|---|
| `CONFIG_SOURCE_DIR`, `CONFIG_GIT_DIR` | `./configs`; отдельные Git-метаданные необязательны |
| `HUB_PORT`, `RELAY_PORT`, `ALLOY_PORT` | `8081`, `8082`, `12345`; внутри сети Hub и relay — `8080` |
| `HUB_BIND_ADDRESS`, `RELAY_BIND_ADDRESS`, `ALLOY_BIND_ADDRESS` | `0.0.0.0`; для локального доступа — `127.0.0.1` |
| `UPSTREAM_URL`, `UPSTREAM_BEARER_TOKEN` | Адрес Hub и необязательный токен ретранслятора |
| `RELAY_NAME`, `RELAY_TIMEOUT`, `RELAY_CACHE_MAX_AGE` | `config-relay`, `5s`, `0s` (без срока); можно ограничить, например `24h` |
| `ALLOY_CONFIG_FILE`, `ALLOY_CONFIG_URL` | `./configs/collector/config.alloy`, `http://alloy_configuration_hub:8080` |
| `ALLOY_ID`, `ALLOY_ENV`, `ALLOY_REMOTE` | `alloy-local`, `prod`, `local` |

Остановка сохраняет тома `alloy_data` и `relay_cache`; `down -v` удаляет их. `.env`, локальные конфиги и секреты игнорируются Git и не входят в Docker-образ.

## Интерфейс и ретранслятор

Шапка показывает коллекторы и ретрансляторы, активные за последние 5 минут, число конфигов и хеш коммита. Интерфейс обновляется каждые 3 секунды; блоки конфига подсвечиваются, Copy копирует чистый текст.

`Sent` — отправка ожидает подтверждения; `Received` — подтверждён хеш или применение; `Error` — ошибка Alloy. `Last synced` фиксирует подтверждение версии, но совпадение хешей само по себе не означает `APPLIED`. Состояние сбрасывается при перезапуске.

Основные GET API: `/api/state`, `/api/config/{name}`, `/api/collector/{id}/config` (также `?format=json`), `/api/collector/{id}/effective-config`. `/healthz` проверяет процесс; `/api/relay` доступен в режиме ретранслятора.

Ретранслятор передаёт конфиги и отчёты, а не телеметрию. При временном сбое upstream выдаёт кеш с учётом `RELAY_CACHE_MAX_AGE`; ошибки доступа не маскируются кешем. Каждый опрос повторяет попытку upstream. Дисковый кеш переживает перезапуск, неотправленные отчёты в памяти — нет.

## Безопасность и разработка

- Встроенной аутентификации и TLS нет: ограничьте доступ сетью или reverse proxy с TLS и аутентификацией. Клиентские ID и атрибуты не подтверждают личность.
- Не храните ключи в `.alloy`: API и кеш содержат полный текст. Читайте секреты на хосте Alloy через `local.file` с `is_secret = true`.
- Для разработки нужен Go 1.26+: `go test ./...`, `go test -race ./...`, `go vet ./...`, `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.

---

## 🚀 Быстрое знакомство

> [!TIP]
> **Запустите Hub без репозитория и коллекторов.** Нужны Docker с Compose и терминал в каталоге проекта. Таблицы сначала будут пустыми.

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

**Открыть → [http://localhost:8081](http://localhost:8081)**

Логи: `docker compose --profile hub logs -f alloy_configuration_hub`

Остановить: `docker compose --profile hub down`
