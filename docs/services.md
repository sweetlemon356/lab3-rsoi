# Основные сервисы варианта 2

Реализованы Reservation, Payment и Loyalty. Gateway остается единственной точкой
координации: основные сервисы не обращаются друг к другу ни по HTTP, ни через БД.

| Сервис | Порт | Домен | Прикладные сценарии | PostgreSQL |
|---|---|---|---|---|
| Reservation | 8070 | Hotel, Stay, Reservation | Каталог, расчет цены, создание, чтение и отмена бронирований | hotels, reservation, reservation_cancellations |
| Payment | 8060 | Payment | Создание, чтение и отмена оплаты | payment, payment_cancellations |
| Loyalty | 8050 | Account, BookingState | Получение уровня, учет и отмена бронирования | loyalty, loyalty_reservations |

## Слои и зависимости

В каждом сервисе используется `adapters -> application -> domain`. Доменные
правила проверяются без сети и БД. HTTP DTO, прикладные модели и представление
данных в SQL разделены. Контроллер получает интерфейс сценариев, прикладной сервис
получает интерфейсы репозиториев. Конкретные реализации собираются конструкторами
в `cmd/<service>/main.go`.

Ниже показана структура Reservation. Payment и Loyalty построены аналогично,
но каждому достаточно одного интерфейса контроллера и одного порта репозитория.
Стрелка зависимости означает использование, пунктир с треугольником — реализацию
интерфейса Go.

```mermaid
classDiagram
    class Controller
    class Catalog {
        <<interface>>
        ListHotels(ctx, PageQuery) HotelPage
        Quote(ctx, hotelUID, start, end, discount) Quote
    }
    class Bookings {
        <<interface>>
        ListReservations(ctx, username) Details[]
        GetReservation(ctx, username, uid) Details
        Create(ctx, CreateCommand)
        Cancel(ctx, username, uid)
    }
    class Service
    class HotelRepository {
        <<interface>>
        List(ctx, PageQuery) HotelPage
        GetHotel(ctx, uid) Hotel
    }
    class ReservationRepository {
        <<interface>>
        ListByUser(ctx, username) Details[]
        GetByUser(ctx, username, uid) Details
        Create(ctx, Reservation)
        Cancel(ctx, username, uid)
    }
    class PostgresRepository
    class Stay {
        StartDate time.Time
        EndDate time.Time
        Nights() int64
        Price(nightlyPrice, discount) int64
    }
    class Reservation {
        UID string
        Username string
        PaymentUID string
        HotelUID string
        Status string
        AcceptReplay(candidate)
        Cancel() Reservation
    }
    Controller --> Catalog
    Controller --> Bookings
    Service ..|> Catalog
    Service ..|> Bookings
    Service --> HotelRepository
    Service --> ReservationRepository
    Service --> Stay
    Service --> Reservation
    PostgresRepository ..|> HotelRepository
    PostgresRepository ..|> ReservationRepository
    Reservation *-- Stay
```

`context.Context` передается от HTTP-запроса в сценарий и далее в pgx: отмена
клиентом или общий deadline прекращают ожидание соединения, SQL и блокировок.
Чистые доменные методы не получают `context`, поскольку не выполняют ввод-вывод.
В `internal/platform` вынесено только повторяющееся техническое поведение:
HTTP, жизненный цикл процесса и SQL-блокировка операции. Доменных моделей там нет.

## Правила и транзакции

- Стоимость считает `Stay`: календарные ночи × цена, затем скидка и округление
  общей суммы до целого рубля, половины вверх. Промежуточные вычисления защищены
  от переполнения. Прошедшие даты допустимы.
- Уровень определяется текущим счетчиком: 0–9 BRONZE/5%, 10–19 SILVER/7%,
  от 20 GOLD/10%. Скидка новой брони берется до увеличения счетчика.
- Команды с одним UUID сериализуются транзакционной advisory-блокировкой.
  Повтор с теми же данными успешен; изменение неизменяемых полей дает `409`.
- Отмена терминальна. Отмена еще неизвестного UUID записывает tombstone или
  строку журнала, поэтому опоздавшее создание не восстанавливает операцию.
- Loyalty блокирует аккаунт `FOR UPDATE` и меняет журнал брони, счетчик,
  уровень и скидку одной транзакцией. Повторный запрос не меняет счетчик дважды.
- Reservation проверяет владельца в SQL; чужое бронирование возвращает `404`.
  Отмена проверяет владельца до любых записей.

Межсервисной транзакции нет. Компенсации Gateway и их ограничения описаны в
[контрактах](gateway-internal-api.md). Сохраняемый координатор восстановления
после падения Gateway пока не реализован.

Отличия SQL от примера задания: деньги хранятся как `BIGINT`, даты проживания
как `DATE`, опечатка `end_data` исправлена на `end_date`. Добавлены ограничения,
индексы и журналы идемпотентности. Это не меняет публичный JSON-контракт.

## Основные команды Make

`make help` показывает все цели. Для повседневной работы:

```sh
make check                         # форматирование, vet, race-тесты и сборка
make tools                         # Goose и Newman в локальном .tools/
make up                            # Compose: сборка, запуск и ожидание готовности
make health                        # health всех четырех сервисов
make test-api                      # исходный Postman: success
make test-failover                 # Postman с остановкой Loyalty в Compose
make test-e2e                      # дополнительные системные сценарии
make logs LOG_SERVICE=gateway      # логи выбранного сервиса
make down                          # остановка с сохранением данных БД
```

`make build` создает `bin/gateway`, `bin/reservation`, `bin/payment`, `bin/loyalty`.
`make run SERVICE=payment` запускает выбранный сервис через Go; основные сервисы
требуют `DATABASE_URL` и предварительно примененных миграций.

```sh
DATABASE_URL='postgres://payment_service:test@localhost:5432/payment?sslmode=disable' make migrate SERVICE=payment
DATABASE_URL='postgres://payment_service:test@localhost:5432/payment?sslmode=disable' make migrate-status SERVICE=payment
```

Для полного нативного прогона достаточно `make tools`, затем `make test-system`.
Нужен установленный PostgreSQL с утилитами и свободные порты 8050–8080 и 55432.
`make test-integration` требует три явно заданных `*_TEST_DATABASE_URL`; при их
отсутствии цель завершается ошибкой, чтобы SQL-тесты не пропускались незаметно.
Обычные `make test` / `make test-race` запускают Go-тесты с учетом переменных среды;
без адресов тестовых БД и системы соответствующие интеграционные тесты пропускаются.

`BASE_URL` меняет адрес Gateway для `make test-api` и `make test-e2e`.
Пути к уже установленным инструментам можно передать как `GOOSE` и `NEWMAN`:

```sh
make test-system GOOSE=/path/to/goose NEWMAN=/path/to/newman
```

`make clean` удаляет только четыре бинарника из `bin/`. `make down` и `make clean`
не удаляют Docker volume. Локальные инструменты и бинарники исключены из Git
и Docker build context.

## Запуск в Docker

```sh
docker compose up -d --build --wait --wait-timeout 120
docker compose logs -f
```

PostgreSQL 13 создает три базы с отдельными владельцами. Подключение чужого
сервиса к базе запрещено через `REVOKE CONNECT`. Контейнер каждого сервиса
выполняет свои SQL-миграции отдельной CLI Goose v3.26.0 и затем запускает бинарник.
Goose не встроен в приложение. Healthcheck проверяет `/manage/health`; основные
сервисы отвечают `200` только при доступной БД. Gateway проверяет собственную
готовность RabbitMQ по состоянию AMQP-соединения. Compose запускает Gateway после
готовности трех HTTP-сервисов и RabbitMQ. Брокер использует volume `rabbitmq-data`,
очереди durable/quorum и persistent-сообщения. Клиент `github.com/rabbitmq/amqp091-go`
закреплен в `go.mod`; AMQP-типы остаются в адаптере и точке сборки.
Подробности доставки и конфигурации — в [gateway.md](gateway.md).

Используется новый volume `db-data-v3`, чтобы исходный volume шаблона с другой
инициализацией не мешал запуску. Старые данные автоматически не удаляются.
Обычная остановка сохраняет данные:

```sh
docker compose down
```

Учетные данные в Compose предназначены для локальной лабораторной работы.
Публичный API — `http://localhost:8080/api/v1`.

## Конфигурация и запуск без Docker

| Переменная | Значение по умолчанию | Назначение |
|---|---|---|
| `HTTP_ADDR` | `:8070`, `:8060`, `:8050` | Адрес соответствующего сервиса |
| `DATABASE_URL` | Обязательна | PostgreSQL URL собственной базы |
| `REQUEST_TIMEOUT` | `5s` | Срок обработки запроса, включая SQL |
| `SHUTDOWN_TIMEOUT` | `10s` | Ожидание завершения запросов при SIGINT/SIGTERM |

При нативном запуске сначала создать роль и базу, применить миграции через Goose,
затем запустить сервис. Например, для уже созданной Payment DB:

```sh
export DATABASE_URL='postgres://payment_service:test@localhost:5432/payment?sslmode=disable'
goose -dir migrations/payment postgres "$DATABASE_URL" up
go run ./cmd/payment
```

Аналогично запускаются Reservation и Loyalty. Gateway: `go run ./cmd/gateway`.
Приложение откажется стартовать без доступной БД и необходимых таблиц.

## TDD и проверки

Работа выполнялась по циклу «тест → красный прогон → реализация → зеленый прогон»:
сначала доменные правила, затем сценарии с тестовыми портами, PostgreSQL-адаптеры
и HTTP. Тесты и код прошли отдельные агентские ревью; замечания исправлены.

```sh
go vet ./...
go test -race ./...
go build ./cmd/...
```

Без переменных `RESERVATION_TEST_DATABASE_URL`, `PAYMENT_TEST_DATABASE_URL`,
`LOYALTY_TEST_DATABASE_URL` SQL-тесты пропускаются. Они требуют отдельных
мигрированных БД с суффиксом `_test`, никогда не используют рабочие базы.
Проверены повторы и конфликты, конкурентное создание/отмена, конкурентный учет
лояльности и откат журнала вместе со счетчиком при искусственной SQL-ошибке.

Полный нативный прогон выполняет [scripts/test-system.sh](../scripts/test-system.sh).
Нужны Go, PostgreSQL с `initdb`/`pg_ctl`/`psql`, curl, Goose и Newman. Инструменты:

```sh
go install -tags='no_clickhouse no_libsql no_mssql no_mysql no_sqlite3 no_vertica no_ydb' github.com/pressly/goose/v3/cmd/goose@v3.26.0
npm install --global newman@6.2.1
./scripts/test-system.sh
```

При необходимости пути задаются через `GOOSE_BIN` и `NEWMAN_BIN`. Скрипт создает
временный кластер на порту 55432 (`TEST_PG_PORT` переопределяет), отдельные рабочие
и тестовые базы, проверяет миграции вверх/вниз, изоляцию БД, vet и race-тесты,
поднимает реальные бинарники на 8050–8080, выполняет исходную коллекцию Postman
и дополнительные системные тесты. Затем выполняет step1–step4 failover
с остановкой и запуском процесса Loyalty и проверяет healthcheck после остановки БД.
После прогона процессы останавливаются; при ошибке сохраняются логи.

Для уже поднятого тестового Compose:

```sh
npx --yes newman@6.2.1 run v2/postman/collection.json -e v2/postman/environment.json
SYSTEM_TEST_URL=http://localhost:8080 go test -v ./tests/system
```

Системные тесты создают бронирования с уникальными пользователями и отменяют
их при завершении. Запускать их следует на тестовом экземпляре системы.
CI проверяет сборку всех образов, запуск Compose, SQL-тесты на PostgreSQL 13,
приемочную коллекцию и системные сценарии.

В текущем окружении полный нативный прогон выполнен на PostgreSQL 18.6:
все проверки прошли, Postman — 8 запросов/8 проверок без ошибок. Docker Compose
прошел проверку конфигурации; сборка образов и запуск контейнеров локально
не проверены, поскольку Docker-демон недоступен.
