# Обновление форка по релизам upstream

Служебная ветка `upstream-sync` форка `overtheground/multica`. Кода продукта
здесь нет: только скрипты и инструкция. Ветка не сливается с остальными.

## Схема

1. Раз в день автопилот (режим `run_only`, исполнитель Opus) запускает
   `check-releases.sh`. Без нового релиза он ничего не создаёт: ни issue, ни
   комментариев.
2. Новый стабильный релиз `multica-ai/multica` (не draft, не prerelease, тег
   `vX.Y.Z`) → одна issue «Обновить форк до Multica vX.Y.Z» на Opus.
3. Opus в этой issue мержит тег в ветку `sync/vX.Y.Z` и ставит `in_review`.
   Уведомление Максу приходит штатно, через Mika.
4. Деплоит внешний установочный агент после решения Макса (раздел ниже).
   Автоматического деплоя нет.

## Состояние

Всё хранится в метаданных issue Multica, в git состояния нет.

Реестр, issue MUL-15 (`01a13c85-9454-73bd-ada3-6c78a2fd104a`):

| Ключ | Значение |
| --- | --- |
| `upstream_sync_kind` | `registry` |
| `upstream_sync_baseline_tag`, `_release_id`, `_published_at` | Последний релиз, который уже считается учтённым. Исходно `v0.6.2`, id `411568605` |
| `upstream_sync_base_branch` | Ветка форка, на которую мержится релиз. Сейчас `mul-19-notification-diagnosis` |
| `upstream_sync_deployed_sha` | Что сейчас работает в продакшене. Сейчас `5e744988164ac27b09a45f11c519fc51c4e5cfc3`. База обязана его содержать, иначе подготовка останавливается |
| `upstream_sync_agent_id` | Исполнитель (Opus) |
| `upstream_sync_last_check_at`, `_last_check_result` | Итог последней проверки: `no-new-release`, `waiting:<tag>`, `issue:<tag>`, `error` |

Issue релиза:

| Ключ | Значение |
| --- | --- |
| `upstream_sync_kind` | `release` |
| `upstream_sync_tag`, `_release_id`, `_published_at` | Релиз |
| `upstream_sync_state` | `queued` → `merging` → `prepared`, или `failed` |
| `upstream_sync_base_branch`, `_base_sha` | От чего строилась ветка (это же точка отката) |
| `upstream_sync_deployed_sha` | Продакшен на момент подготовки |
| `upstream_sync_branch`, `_prepared_sha` | Результат |
| `upstream_sync_error` | Причина при `failed` |

Правила, которые из этого следуют:

- Новым считается релиз, опубликованный позже baseline и позже последнего
  релиза, на который уже есть issue. Исторические релизы не всплывают.
- Если между проверками вышло несколько релизов, issue создаётся на самый
  новый: его merge включает предыдущие. Они перечислены в описании issue.
- Пока есть открытая issue релиза (`backlog`, `todo`, `in_progress`,
  `blocked`), новая не создаётся: параллельных merge нет. Релиз при этом не
  теряется: issue на него появится при первой проверке после закрытия текущей.
  `in_review` и `done` не держат очередь.
- Ошибка GitHub: 4 попытки с паузой, затем запуск завершается без изменений, и
  релиз находится снова на следующий день. Проверить:
  `multica issue metadata list 01a13c85-9454-73bd-ada3-6c78a2fd104a`.
- Оборванная проверка (issue создана, метаданные нет) восстанавливается по
  названию: такая issue лежит в `backlog` и запуска не получила.

## Ежедневная проверка (текст автопилота)

```
Ежедневная проверка стабильных релизов multica-ai/multica. Выполни только это:
1. gh api 'repos/overtheground/multica/contents/check-releases.sh?ref=upstream-sync' -H 'Accept: application/vnd.github.raw' > ./check-releases.sh
2. bash ./check-releases.sh   (на переднем плане, дождись конца)
3. Больше ничего: не пиши комментариев, не создавай и не меняй issue, не делай merge.
Если скрипт завершился ошибкой, запусти его ещё раз. При повторной ошибке закончи
запуск и приведи последние строки вывода: релиз будет найден следующей проверкой.
```

## Подготовка релиза (для Opus в issue релиза)

Ограничения: без деплоя; без PR, issues и контактов с upstream; без
force-push; базовую ветку и чужие worktree не трогать; LICENSE, NOTICE и
FORK.md сохранить. Длинные тесты не запускать: достаточно разрешить конфликты
и собрать backend.

```
multica repo checkout https://github.com/multica-ai/multica
cd multica
git remote get-url fork || git remote add fork https://github.com/overtheground/multica
git fetch fork upstream-sync
git show fork/upstream-sync:prepare-release.sh > ../prepare-release.sh
bash ../prepare-release.sh start <issue-id>
```

`start` проверяет, что тег по-прежнему стабильный релиз на том же коммите и
что базовая ветка содержит `upstream_sync_deployed_sha`. Коммиты базы, ещё не
выкаченные в продакшен, он перечисляет: их надо упомянуть в итоге. Затем
мержит тег (`--no-ff`) в отсоединённый HEAD от `fork/<base_branch>`.

Код выхода 3 означает конфликты. Разрешить, сохранив и наши изменения, и
upstream. Горячие точки перечислены в `FORK.md`, раздел «Taking upstream
updates», п. 5. Миграции:

- миграции только добавляются. Файлы, которые есть в продакшене (включая
  наши 573–583, в том числе исходные 581 и 582), не переименовывать и не
  менять: `finish` это проверяет и останавливается;
- раннер учитывает миграцию по полному имени файла, а не по номеру. Если
  upstream добавил миграцию с номером, который занят нашей, оставить обе;
- в `concurrentIndexCleanups` (`server/cmd/migrate/main.go`) оставить
  ключи обеих сторон;
- при конфликте в `server/pkg/db/generated/` взять версию upstream и
  перегенерировать: `make sqlc`.

Затем `git add -A && git commit --no-edit` и:

```
bash ../prepare-release.sh finish <issue-id>
```

`finish` проверяет: конфликтов нет, HEAD содержит тег, базу и продакшен-SHA,
применённые миграции не тронуты, LICENSE и
NOTICE совпадают с upstream, FORK.md на месте, маркеров конфликта нет. Ещё
он предупреждает о новых совпадениях номеров миграций и выполняет
`go build ./...` в `server/`. После этого пушит ветку `sync/<tag>` и тег
отката `pre-sync/<tag>` (база) обычным push и записывает SHA в метаданные.
Повторный запуск безопасен: уже запушенная ветка `sync/<tag>` переиспользуется.

Итоговый комментарий: ветка, SHA, база, тег отката, что было в конфликтах,
предупреждения. Затем статус `in_review`. Если проблему не решить:
`bash ../prepare-release.sh fail <issue-id> "<причина>"`, комментарий,
статус `blocked`. Повторить попытку: `multica issue rerun <issue-id>`.

## Для внешнего установочного агента

Деплой только по решению Макса. Нужные данные лежат в метаданных issue
релиза: `upstream_sync_prepared_sha` (что ставить) и `upstream_sync_base_sha`
(откуда пришли).

Обновление:

1. Записать текущий задеплоенный SHA: это и есть точка отката. Он может
   отличаться от `pre-sync/<tag>`, если база ушла вперёд после деплоя.
2. Сделать резервную копию базы данных.
3. Собрать backend из `upstream_sync_prepared_sha` (ветка `sync/<tag>`) и
   переключить на него только backend, как описано в `FORK.md`, раздел
   «Deploying a specific version». Миграции применяются при старте.
4. Проверить, что сервис поднялся, и сообщить в issue релиза.
   Обновить реестр: `upstream_sync_deployed_sha` = новый SHA и при смене
   ветки `upstream_sync_base_branch` (команды в «Ручных операциях»). Если у
   установочного агента нет доступа к Multica, это делает Opus по просьбе Макса.
5. После приёмки базовая ветка форка должна включить релиз. Если у
   `upstream_sync_base_branch` нет новых коммитов после `upstream_sync_base_sha`,
   достаточно fast-forward: `git push fork <prepared_sha>:refs/heads/<base_branch>`
   (без `--force`). Если коммиты появились, смержить `sync/<tag>` в базовую
   ветку обычным merge. Это отдельное действие, на него нужно согласие Макса.

Откат:

1. Вернуть backend на SHA из шага 1 (обычно `pre-sync/<tag>`).
2. Если новые миграции upstream уже применены и старый код с ними не
   работает, восстановить базу из копии шага 2. Down-миграции upstream
   использовать только после того, как их прочитал человек.
3. Ветки и теги не удалять: `sync/<tag>` остаётся для повторной попытки.

## Ручные операции

- Пауза: `multica autopilot update <id> --status paused`.
- Другая базовая ветка: `multica issue metadata set 01a13c85-9454-73bd-ada3-6c78a2fd104a --key upstream_sync_base_branch --value <branch> --type string`.
- Новый продакшен: `multica issue metadata set 01a13c85-9454-73bd-ada3-6c78a2fd104a --key upstream_sync_deployed_sha --value <sha> --type string`.
  Деплой ветки, которая не является базой, без обновления обоих ключей приведёт
  к остановке следующей подготовки с понятной ошибкой, а не к потере фиксов.
- Пропустить релиз: закрыть его issue (`cancelled`). Следующий релиз
  подготовится как обычно.
- Проверить без побочных эффектов: `bash check-releases.sh --dry-run`.
