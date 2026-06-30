# PashMan — контекст проекта

Локальный клиент HTTP-запросов («свой мини-Postman»). Один Go-бинарник со
встроенным веб-интерфейсом: при запуске поднимает HTTP-сервер на `127.0.0.1`
(порт 8787, при занятости инкремент до +50), открывает браузер и обслуживает UI.
Запросы выполняет сам бэкенд (поэтому **нет проблемы CORS** — браузер только
рисует интерфейс). Без БД, без внешних зависимостей — только стандартная
библиотека Go.

## Зачем именно так
- **Локальный сервер + браузер**, а не один HTML-файл: иначе браузер блокирует
  произвольные запросы к чужим доменам (CORS). Сервер этим не ограничен.
- **Go single binary**: «один файл, который открывается на macOS и Windows».
  Кросс-платформенность = отдельный бинарник под каждую ОС (это неизбежно).
- Зависимостей нет принципиально — сборка работает офлайн, `go build` без
  скачивания модулей.

## Структура
```
go.mod              module pashman, go 1.22 (собрано go1.26)
main.go             весь бэкенд (stdlib only)
web/index.html      весь UI (vanilla JS, встраивается через //go:embed)
dist/               собранные бинарники + ИНСТРУКЦИЯ.md (для пользователя)
  pashman-win.exe          windows/amd64
  pashman-mac-arm64        darwin/arm64
  pashman-mac-intel        darwin/amd64
collections/        создаётся рядом с бинарником в рантайме (JSON-коллекции)
```

## Бэкенд (main.go)
HTTP-ручки:
- `GET  /`                      — отдаёт встроенный web/index.html
- `POST /api/send`              — один запрос (тело = модель `Request`), вернёт `SendResult`
- `POST /api/batch`             — прогон по CSV: `{request, csv, delimiter, delayMs}` → **потоковый NDJSON**
  (`delayMs` — пауза между запросами, реализована через `select` с `time.After`/`ctx.Done`):
  сначала строка `{"type":"meta","total":N}`, затем по строке `{"type":"row","index":i,"row":{},"result":{}}`
  на каждый запрос. Клиент читает поток в реальном времени; остановка — `AbortController`
  на клиенте обрывает `r.Context()`, цикл это замечает (`ctx.Err()`) и прекращает прогон.
- `GET  /api/collections`       — список *.json в папке collections
- `GET  /api/collection?file=`  — загрузить коллекцию
- `POST /api/collection/save`   — сохранить (`append:false` + `collection`, либо `append:true` + `request`)
- `POST /api/collection/delete` — удалить запрос из коллекции: `{file, index}` → перезаписывает файл без этого запроса
- `POST /api/collection/update` — обновить запрос по индексу «по месту»: `{file, index, request}`
- `POST /api/collection/duplicate` — вставить копию запроса после оригинала: `{file, index}` (имя + « (копия)»)
- `POST /api/collection/rename` — переименовать файл коллекции и её `Name`: `{file, newFile}` (ошибка, если занято)
- Хелперы `loadCol()/writeCol()` — общая загрузка/запись коллекции для update/duplicate/rename.

Ключевые детали:
- Модели: `Request{Name,Method,URL,Headers,Auth,Body,Insecure}`, `Auth{Type(none|basic|bearer),Username,Password,Token}`, `Collection{Name,Requests}`.
- Авторизация: basic → `Authorization: Basic <base64>`, bearer → `Authorization: Bearer <token>`.
- Тело: дефолтный `Content-Type: application/json`, если не задан; не шлётся для GET/HEAD.
- `Insecure` → `tls.Config{InsecureSkipVerify:true}`. Клиентский таймаут 120с.
- `doSend(ctx, Request)` использует `http.NewRequestWithContext` — отмена контекста (через AbortController клиента) прерывает запрос на лету.
- Подстановка переменных: `substitute()` + regex `{{ name }}` (с триммингом). `applyRow()`
  применяет значения CSV-строки к URL, Body, Headers (ключ и значение) и полям Auth.
- CSV: `encoding/csv`, `Comma` = первый символ delimiter, `FieldsPerRecord=-1`
  (допускаем рваные строки), первая строка = заголовки колонок.
- `collectionsDir()` = папка рядом с `os.Executable()` + `/collections`, создаётся при первом обращении.
- `openBrowser()` по `runtime.GOOS`: windows=rundll32, darwin=open, иначе xdg-open.

## Фронтенд (web/index.html)
Одна страница, vanilla JS, тёмная тема. Вкладки: Авторизация / **Параметры** / Заголовки /
Тело (JSON) / Прогон по CSV / Сохранение. Снизу — панель ответа (статус/время/
размер, тело + заголовки, **поиск по телу** с подсветкой `<mark>` через `renderRespBody`).
- **Параметры** — редактор query-строки (`addParamRow`/`collectParams`), двусторонняя синхронизация
  с полем URL: `paramsToUrl()` ↔ `urlToParams()` под флагом `syncingParams`; `encQ/decQ` сохраняют
  плейсхолдеры `{{...}}` нетронутыми (не percent-кодируют).
- **Своё модальное окно ввода `uiPrompt()`** (`#promptModal`) — замена нативного `prompt`; импорт,
  переименование и «в новый файл» используют `uiConfirm`/`uiPrompt`. Оба окна закрываются по `Esc`/`Enter`.
- **Умное сохранение**: `loadedFile`/`loadedIndex` помнят, откуда загружен запрос; «Сохранить»
  (`#saveSmart`) обновляет его по месту (`/update`), иначе добавляет; «Сохранить в новый файл…»
  (`#saveNewFile`) создаёт файл. Верхней кнопки «Сохранить» больше нет. `updateSaveHint()` показывает,
  что сделает кнопка. В списке запросов у каждого: ⧉ дублировать, ✎ переименовать, ✕ удалить;
  рядом с выбором коллекции — ✎ переименовать коллекцию. `refreshCollection()` — общий рефреш.
- Прогон по CSV — поле «Пауза между запросами, мс» (`#delayMs`) и **поиск по результатам**
  (`#batchSearch`, `batchHaystack()` — по всем полям строки/статусу/URL/телу/ошибке).
- Логотип в шапке — просто «PashMan» (без иконки). Верхней кнопки «Сохранить» нет (убрана как дубль).
- Кнопка 📂 (`#openCollectionFile`) — открыть коллекцию из произвольного JSON-файла: `normalizeCollection()`
  приводит содержимое к `{name,requests}` (поддержка массива запросов / одиночного запроса / полной коллекции),
  затем импорт через существующий `/api/collection/save` (append:false) в папку collections и авто-открытие.
  При совпадении имени предлагает (через `uiConfirm`/`uiPrompt`) перезаписать или сохранить под другим
  именем (`uniqueName()` вида `name-2.json`).
- В списке запросов слева у каждого пункта есть кнопка ✕ (`deleteRequest` → `/api/collection/delete`).
  Подтверждение — **собственное модальное окно `uiConfirm()`** (`#confirmModal`), а НЕ нативный `confirm()`:
  браузер мог блокировать повторные нативные диалоги (после import-промптов), из-за чего удаление «молча»
  не срабатывало. После удаления список обновляется через `loadCollectionList(file)` + `onchange()`.
  GET-ответы API отдаются с `Cache-Control: no-store` (и клиент шлёт `cache:'no-store'`), чтобы список
  не брался из кеша. На вкладке «Прогон по CSV» — выбор файла сбрасывает `value` (чтобы выбрать тот же
  файл повторно) и показывает имя; кнопка «✕ Очистить» (`#clearCsv`) чистит CSV, файл и результаты.
- `currentRequest()` собирает модель из полей, `applyRequest()` заполняет поля из модели.
- Прогон по CSV — **потоковый**: `#runBatch` делает `fetch` с `AbortController`, читает NDJSON
  через `reader`, по каждой строке `appendLiveRow()` дорисовывает таблицу (`#liveTable`) в реальном
  времени и обновляет счётчик «выполнено N из M». Кнопка «⏹ Остановить» (`#stopBatch`) вызывает
  `batchAbort.abort()`. По завершении/остановке `renderBatch(lastBatch)` строит выпадающий
  **фильтр по статусу** (группы 2xx/3xx/4xx/5xx, конкретные коды, «ошибки сети»),
  `renderBatchTable()` рисует отфильтрованную таблицу, `matchFilter()` — логика фильтра.
- **Клик по строке** → `openDetail(i)` открывает модалку (#detailModal) с полным
  ответом: переменные строки, итоговый URL, заголовки, полное тело. Закрытие: крестик/фон/Esc.

## Сборка
Go ставился через `winget install GoLang.Go`. Бинарь Go: `C:\Program Files\Go\bin\go.exe`
(после установки PATH в текущей сессии может не обновиться — вызывать по полному пути).
Кросс-сборка из Windows:
```
$go="C:\Program Files\Go\bin\go.exe"; cd <repo>
$env:GOOS="windows";$env:GOARCH="amd64"; & $go build -o dist\pashman-win.exe .
$env:GOOS="darwin"; $env:GOARCH="arm64"; & $go build -o dist\pashman-mac-arm64 .
$env:GOOS="darwin"; $env:GOARCH="amd64"; & $go build -o dist\pashman-mac-intel .
$env:GOOS="";$env:GOARCH=""
```

## Тестирование (важные нюансы окружения)
- Песочница блокирует `Start-Process` и сетевой listen у бинарника
  (`EPERM uv_spawn` / exit 255). Запускать сервер для проверки нужно с
  `dangerouslyDisableSandbox:true`, обычно в фоне (`run_in_background`).
- Фоновый сервер потом останавливать: `Get-Process pashman-win | Stop-Process -Force`
  (в логе фоновой задачи это выглядит как exit 255 — это ожидаемо, не баг).
- httpbin.org бывает медленный/отдаёт 503 — для проверок надёжнее postman-echo.com
  (`/post` эхо тела, `/status/{code}` отдаёт заданный код).
- Проверенные сценарии: single + bearer (200), batch с разделителем `;`,
  подстановка `{{id}}`/`{{name}}` в URL и тело, save new + append, фильтр по статусам.

## macOS-нюанс для пользователя
Бинарники не подписаны → карантин Gatekeeper. Первый запуск:
`chmod +x ./pashman-mac-arm64 && xattr -d com.apple.quarantine ./pashman-mac-arm64 && ./pashman-mac-arm64`.
Подробности — в dist/ИНСТРУКЦИЯ.md (пользовательский гайд на русском).

## Возможные доработки (обсуждались, не сделаны)
- Выгрузка результатов прогона в CSV (строка + статус + ответ).
- История запросов; импорт коллекций из настоящего Postman; переменные окружения `{{base_url}}`.
