# Аудит проекта telegram-ogn-tracker

Дата составления: 2026-05-06. Ветка `main`, исходный HEAD `8d5b093`.
Последний апдейт статусов: 2026-05-07, HEAD `8238ff8`.

Сборка `go build` зелёная, `go vet ./...` без замечаний. Тестов нет.

Приоритеты: 🔴 критично · 🟠 серьёзно · 🟡 средне · 🟢 косметика.

## Сводка статусов

| Тир | Всего | Полностью | Частично | Отложено |
|-----|-------|-----------|----------|----------|
| 🔴 | 3 | 3 (1.1, 1.2, 2.1) | — | — |
| 🟠 | 11 | 10 (1.3, 2.2-2.5, 3.1, 3.2, 4.1, 4.2, 4.3) | 1 (2.6) | — |
| 🟡 | 14 | 10 (1.4, 2.7, 2.9, 2.10, 3.3, 3.4, 4.4, 4.5, 5.2, 5.5) | — | 4 (2.8, 5.1, 5.3, 5.4) |
| 🟢 | 5 | 2 (5.6, 7.3) | — | 2 (6.3, 7.2) · 6.1 без фикса |

---

## 1. Concurrency и потенциальные deadlock-и

### 🔴 1.1 Возможный deadlock в `updateFilter` ↔ APRS callback

`tracker.go:402-410` — `updateFilter` держит `t.mu` и вызывает `t.aprs.Disconnect()`. В этот же момент горутина `runClient` находится внутри `t.aprs.Run(callback)` и callback ждёт `t.mu.Lock()` (`client.go:70`). Если реализация `Disconnect` ждёт завершения callback'а — классический deadlock.

Сейчас вы полагаетесь на то, что `Disconnect()` неблокирующий (только ставит `killed=true`, см. `DECISIONS.md`). Это поведение внешней зависимости — её обновление сломает бот.

**Что делать:** освобождать `t.mu` перед `Disconnect()`. Либо явно задокументировать инвариант «Disconnect не ждёт callback» как контракт `ogn-client` и закрепить тестом.

**✅ Сделано в `7aa8e86`.** `stopTrackingAsync` / `stopRadarAsync` отпускают мьютекс и переносят `Disconnect()` в детачнутую горутину.

### 🔴 1.2 Data race на поле `t.aprs`

`t.aprs` пишется под мьютексом (`updateFilter`, `execTrackOn`, `execAreaOff`, `execRadarOn`, `execRadarSetRadius`), но читается без мьютекса в `runClient:50` и `runRadarClient:561`. Это формальная гонка по Go memory model — `go test -race` загорится.

**Что делать:** передавать `*client.Client` параметром в `runClient(stopCh, aprs)` и `sendUpdates(stopCh, aprs)`. Каждая горутина работает со своим экземпляром, новые горутины запускаются с новым клиентом.

**✅ Сделано в `7aa8e86`.** `runClient`/`runRadarClient` принимают `*client.Client` параметром; `go test -race ./...` зелёный.

### 🟠 1.3 Race на `t.bot`

`t.bot = b` пишется в `RegisterHandlers` без мьютекса. Читается в `sendLandingAlert`, `sendUpdates`, `sendRadarUpdates`, `driverWaitTimeout`. Сейчас работает, потому что `RegisterHandlers` вызывается в `main()` строго до запуска горутин. Но если когда-нибудь появится `bot.RegisterHandlers` после `track_on` — поломается.

**Что делать:** инициализировать `t.bot` в `NewTracker` или сразу после создания, до старта любых горутин.

**✅ Сделано в `d7569a6`.** `NewTracker(b *bot.Bot)` принимает бота при конструкции; `t.bot` фиксируется один раз и больше не пишется.

### 🟡 1.4 saveState под мьютексом

`saveState` пишет JSON и переименовывает файл, удерживая `t.mu`. На медленном диске залипает весь бот. Учитывая, что `saveState` дёргается в `runClient` callback на каждом landing event (`client.go:121`), под нагрузкой это болевая точка.

**Что делать:** делать debounced async save в отдельной горутине через канал, либо хотя бы освобождать мьютекс перед `os.Rename`.

**✅ Сделано в `0dca2fc`.** Маршалинг под мьютексом, I/O в `saveWorker` через буферизованный `chan []byte` (cap 1) — устаревшие снапшоты заменяются актуальными. Финальная синхронная запись в `Shutdown()`.

---

## 2. Логические баги

### 🔴 2.1 `/tz` сломан в Docker-образе

`Dockerfile` использует `alpine:latest` без пакета `tzdata`, и в коде нет `import _ "time/tzdata"`. Поэтому `time.LoadLocation("Europe/Kyiv")` вернёт ошибку `unknown time zone` в проде, и команда `/tz` всегда будет отвечать «Неизвестный часовой пояс».

**Фикс (любой из):**
- `RUN apk --no-cache add ca-certificates tzdata` в Dockerfile, или
- добавить `import _ "time/tzdata"` в `cmd/bot/main.go` (вшить базу в бинарь).

**✅ Сделано в `7aa8e86`.** Добавлен `_ "time/tzdata"` в `cmd/bot/main.go`.

### 🟠 2.2 NPE в `cmdDriverOff` при отсутствии сессии

`commands.go:1109-1118` использует `requireGroupChat`, но не `requireSession`. Дальше `s := t.session; delete(s.Drivers, userID)` — `nil pointer deref`, если сессии нет. Аналогично `cmdAreaOff` (`commands.go:1137`).

**✅ Сделано в `d7569a6`.** Хендлеры переведены на `requireGroupSession`.

### 🟠 2.3 `/remove` работает в личке

`cmdRemove` (`commands.go:385`) проверяет только `requireSession`, без `requireGroupSession`. Любой пользователь в DM может стирать пилотов из чужой группы.

**✅ Сделано в `d7569a6`.** `cmdRemove` теперь под `requireGroupSession`.

### 🟠 2.4 Утечка состояния в `WaitingLanding`

`s.WaitingLanding` — один на сессию. `execDMLanding` ставит флаг для конкретного пилота, но `handleDMLanding` примет локацию от **любого** пользователя в DM в течение 2 минут. Если в окне ожидания случайно другой пилот пришлёт локацию в личке — будет записан как landed.

**Что делать:** хранить `WaitingLandingForUser int64`, проверять `m.From.ID`.

**✅ Сделано в `d7569a6`.** Добавлено поле `WaitingDMLandingFor int64` + `DMLandingExpiry`; `handleDMLanding` сверяет `m.From.ID`.

### 🟠 2.5 Расхождение `/start_session` с README

README обещает «running `/start_session` resets the session». Код (`cmdStartSession` → `cmdStart`) запускает диалог «Продолжить / Новая сессия», как обычный `/start`. Либо документация, либо реализация — что-то одно.

**✅ Сделано в `d7569a6`.** `cmdStartSession` стал безусловным «wipe and restart» — соответствует README.

### 🟠 2.6 Single global session — частично сделано

`Tracker.session *GroupSession` — одна на бота. Если бот добавлен в две группы, любая `/start` во второй вытесняет первую без предупреждения. `isTrusted` всегда возвращает `true`. Нет защиты от того, что незнакомец в любом чате создаст сессию.

**Что делать:** `map[int64]*GroupSession`, плюс allow-list групп через env.

**Статус:**
- ✅ Allow-list групп через `ALLOWED_CHATS` env — `255ccb9` (2026-05-06). Незарегистрированные чаты молча игнорируются; пустой/незаданный env сохраняет старое поведение.
- ⏸ `map[int64]*GroupSession` — отложено. Phase A (`aprs *client.Client` перенесён из `Tracker` в `GroupSession`) лежит на ветке `feat/multi-session` непримерженным. Phase B (sessions map + миграция persistence) и Phase C (auto-resume по сессиям) ждут реальной потребности в multi-group.

### 🟡 2.7 Потеря live-локаций после рестарта

`pilotState` не сохраняет `MessageID`, `Position`, `LowSpeedSince`. После рестарта `MessageID=0`, бот шлёт **новые** live-локации, а старые (24-часовые) висят в чате осиротевшие. Также детектор посадки сбрасывается — пилот, простоявший 80 секунд до рестарта, начнёт отсчёт с нуля.

**✅ Сделано в `0dca2fc`.** `pilotState` хранит `MessageID` и `LowSpeedSince`. После рестарта бот редактирует существующее live-сообщение, детектор продолжает счётчик. Stale `LowSpeedSince` (>5 мин) сбрасывается, чтобы не получить ложный «landed» после длинного даунтайма. `Position` не сериализуем — он восстановится из первого пришедшего бикона.

### 🟡 2.8 `pilotButtons` без лимита кнопок

`client.go:260-307` строит inline-клавиатуру — по строке на каждого летающего и севшего пилота. Telegram лимит — ~100 inline-кнопок на сообщение. У `radarButtons` есть `len(rows) >= 20` — у `pilotButtons` нет.

**⏸ Отложено.** В рабочих сценариях бота кнопок < 20; чинить когда упрёмся.

### 🟡 2.9 `EditMessageLiveLocation` без `Heading=0` для оставшегося направления

`client.go:467-493`: при `Course == 0` и `GroundSpeed > 0` подставляется `heading = 360`. Это хак, потому что Telegram рисует стрелку только при ненулевом heading. Но при следующем апдейте, если course снова 0, пилот «крутанётся» на карте. Минор, но заметно UX-баги.

**✅ Сделано в `0dca2fc`.** `TrackInfo.LastHeading` (int) кэширует последний non-zero `msg.Course` в APRS-callback; `sendUpdates` использует кэш вместо хака `=360`. Стрелка остаётся на последнем известном направлении.

### 🟡 2.10 Хрупкое сравнение ошибок Telegram

В нескольких местах: `strings.Contains(err.Error(), "message is not modified")`. Это ломается при изменении текста ошибки в SDK или локализации.

**✅ Сделано в `d9ffc8d`.** Введён хелпер `isMessageNotModified(err)` — три inline-вхождения сведены к одному.

---

## 3. Безопасность

### 🟠 3.1 `isTrusted` всегда true

`commands.go:25-27` — каждый пользователь Telegram может управлять ботом. Учитывая, что `/debug_wipe` зарегистрирован безусловно — любой может стереть всё состояние. Это в коде обозначено как «dev only», но в проде команда живёт.

**Минимум:** убрать `/debug_wipe` за `os.Getenv("DEBUG")` или вынести из `RegisterHandlers` под флагом сборки.

**✅ Сделано:** `/debug_wipe` зарегистрирован только при `DEBUG=1` (`d7569a6`), а доступ к боту в группах ограничивается `ALLOWED_CHATS` (`255ccb9`). `isTrusted(userID)` пока всегда `true` — на per-user уровне разграничение не нужно.

### 🟠 3.2 Права на `data/session.json`

`os.WriteFile(tmp, data, 0o644)` (`persist.go:121`). На хосте файл читается всеми. Внутри — список пилотов, chat IDs, usernames. Поставить `0o600`.

**✅ Сделано в `d7569a6`.** Атомарная запись с правами `0o600`.

### 🟡 3.3 Бинари в репозитории

В корне лежат `bot` и `telegram-ogn-tracker` (~8 MB каждый). Они в `.gitignore`, но уже закоммичены ранее или попали в working tree. Удалить из истории и убедиться, что не уезжают в образ.

**✅ Сделано в `cbc3b78`.** `bot` удалён, `.gitignore` анкорит `/bot` и `/telegram-ogn-tracker` к корню репо (до этого `bot` ловил каталог `cmd/bot/`).

### 🟡 3.4 Нет валидации OGN ID

`shortID` обрезает до 6 символов, но не проверяет `[0-9A-F]`. Эмодзи или китайские иероглифы в ID попадут в budlist-фильтр и потенциально сломают APRS-парсер.

**✅ Сделано в `d9ffc8d`.** `isValidShortID(s)` — ровно 6 hex-символов; гейтит `execAddDirect`, `cmdMyID`, DM-флоу `/add`. Callsign'ы из OGN-фида не валидируются (доверенный источник).

---

## 4. Архитектура

### 🟠 4.1 `commands.go` — 2034 строки

Один файл с командами, exec-функциями, колбэками, гвардами и driver-таймаутом. Стоит разнести: `handlers_commands.go`, `handlers_callbacks.go`, `handlers_dm.go`, `flows_driver.go`, `flows_radar.go`.

**✅ Сделано в `3cc37c3`.** Разнесено на `commands.go`, `callbacks.go`, `flows.go`, `dm.go`.

### 🟠 4.2 `Tracker` — god object

Граф (`GRAPH_REPORT.md`) подтверждает: 89 рёбер, betweenness 0.508. Единая структура держит APRS, бота, сессию, пользователей, devices, мьютекс. Тестировать невозможно. Любое изменение задевает всё.

**Минимально:** выделить `aprsManager` (создание/Disconnect клиента, рестарт горутин по фильтру), `summaryRenderer` (форматирование), `landingDetector` (чистая функция от потока позиций).

**✅ Сделано в `c115e62` + `98d4bd7`:**
- ✅ `landing.go` (`c115e62`) — `updateLandingState` чистая функция, тестируется отдельно.
- ✅ `renderer.go` (`c115e62`) — `formatTrackText`, `buildSummary`, `buildRadarSummary`, `pilotButtons` вынесены в свободные функции с явными зависимостями.
- ✅ `types.go`/`util.go` (`98d4bd7`) — данные и pure helpers вынесены отдельно.
- ✅ APRS lifecycle консолидирован в `client.go` (`98d4bd7`) — `updateFilter`, `stopTrackingAsync`, `stopRadarAsync` переехали из `tracker.go`. `tracker.go` сжат с 991 → 552 строк, остался только orchestrator (Tracker, NewTracker, Shutdown, RegisterHandlers, DefaultHandler).

### 🟠 4.3 Нет тестов

`shortID`, `distanceAndBearing`, `bearingName`, `nearestDriver`, `formatDDBInfo`, `commandArgs`, логика детектора посадки, миграция legacy state — всё чистые функции, легко тестируемые. Сейчас 0% покрытия. Регрессии ловятся прод-юзерами.

**✅ Базовое покрытие добавлено** в `d7569a6`, `255ccb9`, `0dca2fc`, `d9ffc8d`, `98d4bd7`. Покрыты: `shortID`, `commandArgs`, `bearingName`, `distanceAndBearing`, `nearestDriver`, `formatDDBInfo`, `updateLandingState`, `parseAllowedChats`, `isAllowedChat`, `saveState` (async/replace/shutdown), `staleLowSpeedReset`, `isValidShortID`, `isMessageNotModified`, `nextReconnectDelay`, `buildFilter` (7 кейсов), `chooseHeading` (6 кейсов). Покрытие интеграционных путей (cmd*/exec*) по-прежнему отсутствует — нужны e2e-харнесы вокруг `bot.Bot`.

### 🟡 4.4 Нет структурного логирования

`log.Printf` повсюду. Нет уровней, нет полей. Префиксы `[OGN raw]`, `[cmd]`, `[landing]` — текстовый namespace. С `slog` (stdlib с Go 1.21) можно бесплатно получить JSON и фильтрацию.

**✅ Сделано в `7088caf`.** ~157 `log.Printf/Println` мигрированы на `slog.Info/Warn/Error/Debug` со структурированными полями (`chat_id`, `user_id`, `ogn_id`, `lat`, `lon`, `err`). Префиксы заменены уровнями. `cmd/bot/main.go` ставит default handler и редиректит stdlib `log` через slog. `DEBUG=1` включает Debug-уровень. `6a7d8c5` добавил диагностические debug-логи для трассировки OGN beacon-флоу.

### 🟡 4.5 Linting / pre-commit

Нет конфига `.golangci.yml`, нет `staticcheck`, нет CI-шага с линтерами. Только `make vet` локально.

**✅ Сделано в `a2e298b`.** `.golangci.yml` с `errcheck/gosimple/govet/ineffassign/staticcheck/unused/gofmt/goimports/misspell`; CI-workflow на `pull_request`/`push` с jobs `test` (vet + race tests) и `lint`.

---

## 5. Операционная зрелость

### 🟠 5.1 Нет healthcheck / metrics

Нет HTTP-эндпоинта `/health`, нет Prometheus. На деплое непонятно, жив ли OGN-коннект (только по логам).

**⏸ Отложено.** Пока обходимся структурированными slog-логами и debug-heartbeat'ом в `sendUpdates` (`6a7d8c5`).

### 🟡 5.2 Reconnect без backoff

`reconnectDelay = 5 * time.Second` (`client.go:24`) — фиксированный. При длительном падении OGN сервера долбим каждые 5 секунд.

**✅ Сделано в `d9ffc8d`.** Экспоненциальный backoff `1s → 60s cap` для `runClient` и `runRadarClient`; sleep заменён на `select { stopCh / time.After }` чтобы Shutdown прерывал ожидание.

### 🟡 5.3 Нет rate-limiting Telegram API

При 30 пилотах `sendUpdates` шлёт ~30 `EditMessageLiveLocation` + 1 summary каждые 30с. Telegram лимит — 30 msg/sec на бота. Сейчас в пределах, но впритык; на большом радаре уже опасно.

**⏸ Отложено** до первого появления 429 в логах. Библиотека `go-telegram/bot` сама retry-ит.

### 🟡 5.4 Live-локация умирает через 24ч

`liveLocationPeriod = 86400` — после суток бот не пере-создаёт live-сообщение. Длительные сессии теряют отслеживание.

**⏸ Отложено.** Типичная сессия трекинга — несколько часов, на этих сценариях не задевает.

### 🟡 5.5 Graceful shutdown неполный

`main.go` ловит SIGINT/SIGTERM и отменяет `ctx`, но `StopCh` сессии не закрывает — горутины продолжают крутиться до выхода процесса. На продакшене с docker-restart это нормально, но `saveState` после shutdown не вызывается, последние изменения могут потеряться.

**✅ Сделано в `0dca2fc`.** `Tracker.Shutdown()` ставит `shuttingDown=true`, закрывает session/radar `StopCh`, дренит save-channel, делает финальный синхронный write, дисконнектит APRS. `main.go` вызывает после `b.Start`.

### 🟢 5.6 Версионирование Go

`go.mod` — `go 1.23.8`, Dockerfile — `golang:1.23.10-alpine`. Расхождение незначительное, но имеет смысл синхронизировать через `go-version-file: go.mod` (как в release.yml).

**✅ Сделано в `cbc3b78`.** `go.mod` поднят до `1.23.10`; release.yml уже использует `go-version-file: go.mod`, теперь все три источника синхронны.

---

## 6. CI/CD

### 🟢 6.1 Release pipeline хороший

Multi-arch builds, GHCR push, semver-теги, deploy опционален через GitHub var. Solid.

**N/A** — фикса не требуется.

### 🟡 6.2 Нет PR-проверок

Нет workflow на `push` / `pull_request` с `go vet`, `go test`, `golangci-lint`. Релизный workflow собирает только по тегу.

**✅ Сделано в `a2e298b`.** `.github/workflows/ci.yml` — две job'ы (`test` и `lint`) на `pull_request` и `push` в `main`.

### 🟡 6.3 Deploy на `latest`

Сценарий деплоя тянет `:latest`. Откатиться можно только пересборкой/тегом. Лучше пинить версию через переменную.

**⏸ Отложено.**

---

## 7. Документация

### 🟡 7.1 README устарел

Не упомянуты: `/area`, `/area_off`, `/radar`, `/driver`, `/driver_off`, `/tz`, `/myid`, DM-флоу, reply-клавиатура. Описание `/start_session` не соответствует реализации. Утверждение «APRS server filtering is disabled when short ID is added» неверно — короткие ID разворачиваются в `FLR/OGN/ICA/NAV/FNT`-варианты.

**✅ Сделано в `cbc3b78`.** README переписан: актуальный набор команд, документированы `ALLOWED_CHATS` и `DEBUG`, удалён неверный пункт про APRS-фильтрацию.

### 🟢 7.2 DECISIONS.md почти пустой

Две записи. Имеет смысл фиксировать решения по single-session, single-mutex, выбору ogn-client и т.п.

**⏸ Отложено.**

### 🟢 7.3 Комментарии русско-английские

В `tracker.go` смешаны языки в комментариях. Не критично, но снижает читаемость.

**✅ Сделано в `cbc3b78`.** Doc-комментарии переведены на английский во всех файлах пакета `tracker`. UI-строки (кнопки, сообщения) остались русскими.

---

## Что осталось

### 🟠 серьёзно (частично)
- **2.6** — multi-session map (Phase A на ветке `feat/multi-session` непримерженный). Ждёт реальной потребности в multi-group; allow-list (главный security-риск) уже в main.

### 🟡 средне (4 пункта)
- **2.8** — лимит inline-кнопок `pilotButtons`. Ждёт реальной группы с >20 пилотами.
- **5.1** — `/health` эндпоинт + Prometheus метрики.
- **5.3** — rate-limiting Telegram API. Ждёт первого `429` в логах.
- **5.4** — пере-создание live-локации после 24-часового лимита.

### 🟢 косметика (2 пункта)
- **6.3** — deploy не на `:latest`, а на пинованную версию.
- **7.2** — наполнить `DECISIONS.md`.
