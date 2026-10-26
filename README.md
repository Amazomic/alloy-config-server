# Alloy Configuration Hub

[English](README.en.md) · Русский

Hub раздаёт конфигурации Grafana Alloy по атрибутам коллекторов. Alloy забирает их через `remotecfg`, а в веб-интерфейсе видны коллекторы, конфиги и события.

```text
configs/*.alloy → Hub ← Alloy
                      ← Relay ← Alloy (при необходимости)
```

Relay передаёт конфигурации и отчёты между Alloy и Hub и хранит кеш на случай недоступности Hub. Метрики, логи и трейсы Alloy отправляет напрямую в настроенные системы хранения.

![Интерфейс Alloy Configuration Hub](docs/images/hub-dashboard.jpg)

## Запуск Hub

Нужны Docker и Compose. В каталоге проекта:

```bash
git submodule update --init --recursive
docker compose --profile hub up -d --build
```

Интерфейс: [http://localhost:8081](http://localhost:8081).

Конфигурации лежат в `configs/` — это подключённый репозиторий с шаблонами. Измените `.alloy`-файлы под свою инфраструктуру, в том числе адреса назначения телеметрии. Hub автоматически перечитывает файлы; перезапуск не нужен. Обновление файлов из Git выполняется отдельно.

## Подключение Alloy

Добавьте в локальный `config.alloy`:

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

Замените `hub-server` на IP или имя сервера с Hub. Если Alloy запущен на том же хосте вне контейнера, можно указать `http://localhost:8081`. Атрибуты задаются прямо в конфиге; переменные окружения не нужны.

Запустите или перезапустите Alloy с этим файлом. Коллектор появится в интерфейсе после первого обращения.

## Как выбираются конфиги

Hub читает `.alloy`-файлы в корне `configs/`. Имя файла задаёт атрибут и значение:

| Файл | Атрибут Alloy |
|---|---|
| `env_prod.alloy` | `env = "prod"` |
| `remote_local.alloy` | `remote = "local"` |
| `job_grafana.alloy` | `job = "grafana"` |

Все подходящие файлы объединяются внутри `declare "node_stack"` и применяются через `node_stack "default"`. `{{name}}` в шаблонах заменяется атрибутом `name`, а если его нет — ID коллектора.

Выбирайте совместимые шаблоны: `env_prod.alloy` и `job_grafana.alloy` содержат одинаковые имена компонентов и рассчитаны на разные варианты сбора. Синтаксис итогового конфига проверяет Alloy.

## Relay — при необходимости

На сервере ретранслятора создайте `.env` с адресом Hub:

```dotenv
UPSTREAM_URL=http://hub-server:8081
```

```bash
docker compose --profile relay up -d --build
```

В `remotecfg` на коллекторах укажите адрес ретранслятора: `url = "http://relay-server:8082"`. При временной недоступности Hub Relay отдаёт последний сохранённый конфиг. Кеш переживает перезапуск.

## Дополнительно

- Для тестового Alloy в том же Compose: `docker compose --profile hub --profile alloy up -d --build`. Он использует готовый `configs/collector/config.alloy`; настройки — в [.env.example](.env.example). Сбор метрик хоста через cAdvisor рассчитан на Linux.
- По умолчанию используется `./configs`. Другой каталог при необходимости задаётся через `CONFIG_SOURCE_DIR` в `.env`; остальные настройки портов и кеша также перечислены в [.env.example](.env.example).
- В интерфейсе доступны светлая и тёмная темы, просмотр и копирование конфигов. `Sent` — ожидается подтверждение, `Received` — конфиг подтверждён, `Error` — ошибка применения. Отключённый коллектор остаётся неактивным 5 минут, затем исчезает; без явного отключения неактивность определяется после 5 минут без запросов.
- Встроенной аутентификации и TLS нет: размещайте Hub в закрытой сети или за защищённым reverse proxy. Секреты храните на хостах Alloy, а не в раздаваемых конфигах.
- Разработка: Go 1.26+, `go test ./...`, `go test -race ./...`, `go vet ./...`.
