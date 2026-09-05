# Personal Memory: единое ревью корректности контекста

Дата задания: 2026-09-04. Проверенный commit: `956e03cbf6e196a33a866db91b886d19cb94daae`, ветка `main`.

## Статус документа

Это каноническая объединённая версия двух редакций аудита от 2026-09-04. Она сохраняет воспроизводимые находки раннего независимого отчёта и дополняет их расширенной проверкой путей `import`/`export`, lifecycle-связей, ограниченного candidate pool, cache/expiry, счётчиков и валидации ответов Qdrant. Внешняя worktree-копия остаётся исторической ранней редакцией и не является вторым независимым аудитом.

Соответствие восьми находок ранней редакции: F1 → F01; F2 → F02; F3 → F03; F4 → F05; F5 → F04; F6 → раздел R1 и F06; F7 → F07; F8 → F08. Дополнительные findings F09–F14 проверены в расширенной редакции тем же зафиксированным commit и локальным диагностическим стендом.

## Главный вывод

Система не гарантирует, что успешный ответ содержит согласованный, актуальный и достаточно полный контекст. Наиболее опасны три воспроизводимых пути: старый факт остаётся в кэше после успешного обновления; повторное сохранение незаметно заменяет lifecycle и происхождение существующего факта; поиск документов смешивает завершённое старое и незавершённое новое поколение. Во всех трёх случаях следующий читатель получает обычный успешный ответ.

Другая категория риска — доказательство отсутствия из ограниченного поиска. `No facts found.` может означать, что все первые 20 кандидатов отсеяны, хотя подходящий факт находится сразу за ними. Непустая выдача документов может содержать только слабое совпадение в выбранной папке, без поиска в остальных папках. Ограниченность этих алгоритмов документирована; отсутствие в ответе сведений о покрытии делает её плохо различимой для агента.

Здесь подтверждается поведение сервера и потеря информации на его границе. Утверждение «конкретная модель приняла это за достоверное» **не проверялось**: живые агенты и production не запускались. Условия такого принятия: агент опирается на успешность инструмента, `current_truth`, `state:current` или фрагмент документа, не проверяет первоисточник и не получает дополнительного противоречащего свидетельства. Клиентский контракт требует осторожности, но не может восстановить скрытые сервером ошибки, сроки и поколения.

## Область, состояние дерева и метод

Прочитаны предоставленные глобальные инструкции и актуальный корневой `AGENTS.md`. Вложенных `AGENTS.md` в репозитории не найдено. Исследование разрешено пользователем целиком; HTML-план и согласование исправлений не создавались. Исправления не выполнялись.

Начальное состояние: три tracked-изменения в HTML-планах и неотслеживаемые планы, спецификации и `.DS_Store`. Полный начальный `git status --porcelain=v1 -uall` приведён в приложении. Изменённые tracked-файлы:

- `docs/ai-plans/2026-07-19-service-hardening-review-findings.html`;
- `docs/ai-plans/2026-07-20-embedding-model-identity.html`;
- `docs/ai-plans/2026-08-16-lifecycle-provenance-history-viz.html`.

Код изучен в рабочем репозитории; тесты выполнялись в отдельной временной распаковке `git archive HEAD`. Проверяемые Go-исходники и `go.mod`/`go.sum` соответствовали commit. Дополнительные тесты и защитный `TestMain` добавлялись только во временную копию. `.env`, production-конфигурация и настоящие документы не использовались. Факты в память не записывались; production recall также не вызывался, поскольку он меняет счётчики.

Из старых локальных заметок использован только указатель на прежние retrieval-эксперименты. Их результаты не являются доказательством этого ревью. Например, фактический код уже поддерживает `mode=blended`, вопреки описанию в AGENTS.md; прежнее описание счётчиков как атомарных также не соответствует текущему `Get` → `SetPayload`.

Приоритеты: **P1** — устранить до доверия к сервису как источнику операционного контекста; **P2** — воспроизводимая ошибка или существенный пробел контракта при описанных условиях; **P3** — локальная ошибка отбора с непрямым влиянием. Приоритет не означает доказанную частоту в production.

## Подтверждённые находки

### F01 — P1. Кэш возвращает удалённый старый факт после успешного update; ошибки мутаций также оставляют stale-контекст

**Код:** [internal/memory/server.go:1040–1062](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:1040), `788–796`, `1156–1160`, `831–838`; [internal/memory/cache.go:40–67,96–107](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/cache.go:40).

**Условия и цепочка:** `update_fact` создаёт новый ID, инвалидирует кэш, затем отдельным запросом удаляет старый ID. Чтение между инвалидацией и удалением видит оба факта и публикует их в уже новом поколении кэша. Удаление завершается, update сообщает успех, но второй инвалидации нет. Следующий, начатый уже после успеха update, recall возвращает старый текст как `current_truth` до TTL или другой инвалидации.

Дополнительный вариант: Qdrant применил `store`/`delete`, но вернул ошибку или потерял ответ. Обработчики выходят до инвалидации. Кэш продолжает возвращать удалённое содержимое либо пустой ответ вопреки реально выполненной записи. У `set_fact_lifecycle` этот случай уже обработан иначе: инвалидация выполняется после отправки независимо от ошибки (`1113–1118`).

**Последствия:** агент может действовать по уже отменённой настройке или повторить запись, ошибочно решив, что первая не появилась. Ошибка мутации сама по себе видна, но последующее «проверочное» чтение может вводить в заблуждение.

**Воспроизведение:** создать `old setting`, задержать HTTP-обработчик удаления старого ID, запустить update, выполнить recall в этой паузе, разрешить удаление, дождаться успешного update, повторить recall. Старого ID в mock-хранилище уже нет, в ответе он есть. Тесты: `TestReviewUpdateSuccessfulButCacheRetainsDeletedOldFact`, `TestReviewAmbiguousDeleteLeavesCachedFact`, `TestReviewAmbiguousStoreLeavesCachedEmpty`.

**Попытка опровержения:** generation-aware singleflight действительно запрещает публикацию лидера, начавшегося **до** инвалидации; существующий `TestRecallFactsInvalidationPreventsStaleLeaderPublishAndWakesWaiter` это проверяет. Наш читатель начинает работу **после** неё, поэтому защита не действует. Namespace-lock сериализует писателей, но не читателей. Это не просто допустимое пересечение чтения с записью: неправильный ответ сохраняется для чтения после завершения записи.

**Минимальное исправление:** инвалидировать после последнего изменения видимости, включая неоднозначные результаты отправленных мутаций. Описать частичный update и возможность двух записей; для строгой согласованности убрать промежуточную видимость через стабильный ID/версию.

### F02 — P1. `store_fact` может перезаписать существующую запись и потерять lifecycle/provenance

**Код:** [internal/memory/server.go:83–91,694,725–793](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:83); [internal/memory/related.go:23–41](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/related.go:23); [internal/memory/id.go:22–36](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/id.go:22); [internal/qdrant/client.go:333–344](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/qdrant/client.go:333).

**Условия:** в namespace уже существует детерминированный ID для того же точного текста. Два воспроизводимых пути:

1. Запись `superseded`: она специально не блокирует dedup. Повторный `store_fact` получает тот же ID и upsert заменяет её на legacy-current, если lifecycle не передан.
2. Запись current/canonical: оба семантических preflight-запроса завершаются ошибками. Код продолжает запись. Предшествующий точный `Get` проверил только maintenance-active, проигнорировав существование самой записи.

**Цепочка и последствия:** создаётся новый payload с новым `created_at`, нулевым счётчиком, переданными/пустыми tags, без прежних `canonical`, provenance, verification, expiry и отношений. Upsert по тому же ID заменяет запись. Ответ — `status:stored`, без предупреждения об overwrite. История теряется, прошлое утверждение может снова стать текущим; текущая canonical-запись может потерять источник и авторитет.

**Воспроизведение:** для первого пути сохранить synthetic `setting A` с его вычисленным ID и `superseded_by:["42"]`, повторить store без lifecycle. Для второго — current/canonical с provenance и два HTTP 502 на поиск. Тесты: `TestReviewSupersededSameTextOverwritesHistoricalRecord`, `TestReviewSearchFailureOverwritesExistingCanonicalFact`.

**Попытка опровержения:** namespace-lock не помогает — сценарий последовательный. Запрет overwrite для `update_fact` и защита quarantined-ID существуют, но в этом пути отсутствует защита **active** exact-ID. Исключение superseded из dedup оправдано для создания новой версии, но не оправдывает замену старой записи по тому же ID. Fail-open выбран явно; скрытая потеря metadata — отдельное следствие этого выбора.

**Минимальное исправление:** exact-ID collision должен иметь отдельный исход, не зависеть от семантического поиска и не стирать metadata. Возврат к прежнему тексту — явный lifecycle-переход или новая версия со стабильной идентичностью истории. На ошибках preflight возвращать degraded/inconclusive, если сохранение всё же разрешено.

### F03 — P1. Поиск RAG выдаёт одновременно старое и частично записанное новое поколение

**Код:** [internal/rag/indexer.go:239–269](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/indexer.go:239); [internal/rag/server.go:165–183,250–263,306–335](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/server.go:165).

**Условия и цепочка:** один документ уже проиндексирован; новая версия содержит несколько chunks. Новые ID включают content hash, записываются последовательно и сразу доступны поиску. На втором upsert происходит ошибка. Старое полное поколение сохраняется, но первый chunk нового остаётся тоже. Ни hierarchical, ни flat, ни blended не фильтруют подтверждённое активное поколение.

**Последствия:** результат может содержать несовместимые версии одного правила, а несколько версий одного chunk могут вытеснять другие документы из top-k. Ответ не содержит `generation`, `file_hash`, `indexed_at`, `total_chunks` или состояния индекса. Даже сравнить свежесть двух версий по контракту инструмента нельзя. Смешение существует и во время успешной индексации, а после сбоя может длиться до следующего успешного запуска.

**Воспроизведение:** old-generation с одним chunk; новая версия длиннее лимита; отказ второго upsert; передать реально оставшиеся точки поисковому обработчику. Обычный успешный JSON включает оба поколения без маркировки. Тест: `TestReviewPartialGenerationReturnedWithoutVersion`. Это тест индексатора плюс обработчика выдачи на управляемых результатах поиска, без реального Qdrant ranking.

**Попытка опровержения:** generation-ID и сохранение старых данных предотвращают потерю последней полной копии. Они не обеспечивают атомарную публикацию. Успешное завершение следующего запуска действительно удаляет старое поколение; проблема остаётся в промежутке и при отсутствии такого запуска. В blended dedup выполняется по ID, а у поколений ID различаются.

**Минимальное исправление:** staging поколения и явный commit/active manifest, который использует reader. Выдавать версию, время индексации и состояние freshness; при неполной публикации отдавать старую согласованную версию с признаком stale.

### F04 — P2. Индексатор может завершиться успешно, не проиндексировав ни одного файла из-за ошибки

**Код:** [internal/rag/indexer.go:85–109,119–133](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/indexer.go:85); [cmd/indexer/main.go:45–49](/Users/dzarlax/Projects/Code/Personal/personal_memory/cmd/indexer/main.go:45); [internal/rag/server.go:414–426](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/server.go:414).

**Условия и цепочка:** `indexFile` получает ошибку чтения/embedding/upsert. Callback пишет warning и возвращает nil; ошибка не добавляется в `reconcileErrors`. Если `changed=false`, папка не отмечается dirty. В конце `Run` возвращает nil. Аналогично walk-ошибки могут лишь подавить cleanup, не сделав весь запуск ошибочным.

**Последствия:** cron/оператор видит успешное завершение, поиск продолжает обслуживать старый или пустой индекс. У MCP нет terminal-status для background reindex; после начала операции состояние частичного сбоя доступно только через логи. Это не означает, что сам ответ `Reindex started` лжёт о завершении — он корректно сообщает лишь о запуске.

**Воспроизведение:** свежая временная папка с `note.txt`, TEI mock возвращает 503, snapshot chunks/folders пуст. `Run()==nil`, points остаются пустыми. Тест: `TestReviewRunSuppressesIndexError`.

**Попытка опровержения:** ошибки snapshot, cleanup delete и reconcileFolder уже возвращаются. Проверен именно file-error до изменения chunks, где эти другие ветки не компенсируют потерянную ошибку.

**Минимальное исправление:** агрегировать file/walk failures в результат Run; отдельно отражать skipped cleanup и индексированные/неудачные файлы. Для MCP — идентификатор запуска и доступный итог, не только сообщение о старте.

### F05 — P2. Сбой обновления сводки папки не исправляется повторным reindex неизменённых документов

**Код:** [internal/rag/indexer.go:73–81,102–105,125–133,210–223,309–331](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/indexer.go:73).

**Условия и цепочка:** chunks успешно обновлены; upsert folder summary не удался. Следующий запуск видит полное поколение chunks и возвращает `changed=false`. `foldersNeedingRefresh` смотрит только существующие сводки и их `summary_version`, а не отсутствие сводки для известной папки или её отставание от контента. Папка не попадает в dirtyFolders.

**Последствия:** новая папка может постоянно отсутствовать в первой стадии поиска; старая сводка — постоянно маршрутизировать по устаревшим темам. Если другие папки дали непустые hits, flat fallback не спасёт.

**Воспроизведение:** одна новая папка, успешный chunk upsert, 502 для folder upsert; убрать ошибку и вызвать Run второй раз без изменения файла. Второй Run успешен, второй попытки folder upsert нет, сводка отсутствует. Тест: `TestReviewFolderFailureNotRetried`.

**Попытка опровержения:** refresh старого `summary_version` работает и покрыт существующим тестом. Наш случай — отсутствующая сводка; для уже существующей актуальной версии с устаревшим содержимым отсутствует и сравнение content digest. Простой повтор не исправляет состояние, редактирование документа исправит.

**Минимальное исправление:** сверять полный ожидаемый набор папок с фактическим и привязать summary к digest входных файлов; сохранять необходимость повторного reconcile независимо от changed-флага chunks.

### F06 — P2. Истёкший факт блокирует продление, а duplicate-ответ скрывает его срок

**Код:** [internal/memory/related.go:31–45,64–73](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/related.go:31); [internal/memory/server.go:627–647,759–766,201–221,1010–1038](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:627).

**Условия и цепочка:** expired current/legacy факт совпадает выше dedup threshold с новым утверждением. Проверка duplicate предшествует expiry и не исключает истёкшее. Store с новым `valid_until` возвращает duplicate, не сохраняет обновление. `RelatedFactCandidate` не содержит `valid_until`, поэтому duplicate выглядит как valid current. Обычный recall при этом его исключает.

**Последствия:** агент, следуя правилу «duplicate означает информация уже есть», может закончить сохранение, хотя рабочего текущего контекста нет. `update_fact` также не предоставляет поля для смены срока и сохраняет прежний payload; metadata-only lifecycle transition expiry не меняет.

**Воспроизведение:** существующий `setting A`, `valid_until=2000-01-01`; новый store того же текста с `valid_until=2099-01-01`. Получено duplicate без expiry, recall пуст. Тест: `TestReviewExpiredDuplicateBlocksRenewalWithoutShowingExpiry`.

**Попытка опровержения:** предотвращение дублей исторических записей может быть намеренным. Дефект здесь — отсутствие различения «существует expired запись» и «текущая информация уже сохранена» плюс отсутствие обычного renewal-пути. Для superseded исключение уже есть; expired current остаётся блокирующим.

**Минимальное исправление:** включить expiry в duplicate-ответ, явно сообщать необходимость renewal, добавить точное обновление срока. Не превращать продление в неявную потерю истории через delete/store.

### F07 — P2. Срок действия расходится между API, кэшем и представлением inventory

**Код:** [internal/memory/server.go:499–512,831–838,887–920,1441–1457,1635–1638](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:499); [internal/memory/lifecycle_recall.go:225–240](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/lifecycle_recall.go:225); [internal/memory/cache.go:53–67](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/cache.go:53).

**Условия и цепочка:**

- В день `valid_until` recall использует включительную UTC-дату и возвращает факт; operational context и `find_related` используют `isExpired`, сравнивающий текущее время с полуночью начала того же дня, и уже исключают его.
- Current/history/include_all cache key не содержит текущей даты. Cache-hit проверяет TTL и увеличивает счётчик, не пересчитывая expiry. Запись, прочитанная перед UTC-полуночью, остаётся видимой после окончания срока до TTL.
- `list_facts` намеренно возвращает expired inventory, но текст не содержит срока или признака expired, а lifecycle отображается как `state:current legacy`. `valid=true` относится только к lifecycle, не к текущей пригодности факта.

**Последствия:** один агент получает разный набор текущих фактов через стартовый контекст и recall; другой принимает inventory за актуальную память. Cache-ответ ещё и увеличивает видимый recall_count устаревшего факта.

**Воспроизведение:** payload с сегодняшним UTC `valid_until`: `isExpired==true`, `factExpiredAt(..., now)==false`. Для фиксированного `2026-09-04` presentation включает факт в 23:59:59 и исключает в 00:00:01, тогда как тот же key/неистёкший cache entry остаётся доступным. Это **компонентное доказательство** перехода даты, не ожидание настоящей полуночи в живом HTTP-тесте. Отдельный handler-тест подтверждает отсутствие expiry в list. Тесты: `TestReviewExpiryDisagreesAndCacheCrossesDate`, `TestReviewListExpiredFactWithoutExpiryWarning`.

**Попытка опровержения:** даты разных `as_of` изолированы корректно; дефект касается меняющейся текущей даты. TTL ограничивает длительность stale-cache, но не устраняет нарушение срока. Показывать expired inventory допустимо; показывать его без expiry-квалификатора опасно и не требуется контрактом.

**Минимальное исправление:** единая функция expiry и одна UTC-reference date на запрос, одинаковая включительность; cache key с датой или TTL до ближайшей границы; возвращать `valid_until`, `expired_at_reference`, `reference_date` на всех поверхностях с историей.

### F08 — P2. Import пропускает malformed expiry, reader превращает его в бессрочный current

**Код:** [internal/memory/server.go:1230–1269,1355–1357](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:1230); [internal/memory/lifecycle_recall.go:225–236](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/lifecycle_recall.go:225); для сравнения store validation — `server.go:710–714`.

**Условия и цепочка:** импорт получает `valid_until` неправильного типа/формата. Валидация проверяет text/tags/namespace/lifecycle, но копирует expiry как есть. Parse lifecycle не отвечает за это поле. Reader при ошибке разбора срока возвращает `false` из проверки expired.

**Последствия:** временный факт с непригодной датой становится текущим без предупреждения и без срока в recall-ответе. Это fail-open по validity, а не только ошибка отображения.

**Воспроизведение:** import `[{"text":"temporary setting","namespace":"projects","valid_until":"2000-99-99"}]`; затем recall. Факт успешно импортирован и возвращён как `current_truth`. Тест: `TestReviewImportMalformedExpiryAcceptedAsCurrent`.

**Попытка опровержения:** обычный store действительно запрещает такую дату. Но импорт — самостоятельная внешняя write-поверхность, а ошибочные поля не входят в lifecycle validation. Нужда в совместимости с legacy отсутствующим сроком не требует считать malformed присутствующий срок корректным.

**Минимальное исправление:** общий validator expiry для store/import/update; раздельные состояния absent, valid и malformed; malformed записи должны быть inspectable, но не автоматически current truth.

### F09 — P2. Изменяемые ID разрывают существующие supersession-связи; export/import legacy не сохраняет граф

**Код:** [internal/memory/server.go:956,996–1006,1040–1059,1251–1264,1609–1612](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:956); [internal/memory/lifecycle/model.go:267–297,378–409](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/lifecycle/model.go:267).

**Условия и цепочка:** A — superseded и ссылается на текущий B по `superseded_by`. Текст B исправляют через `update_fact`; его ID пересчитывается из нового текста, старый удаляется. Входящие ссылки A не переводятся на новый ID. Lifecycle A остаётся `valid`, поскольку parser проверяет форму, а не наличие цели.

Второй путь: export отдаёт только payload, без source point ID. Import вычисляет новые namespace+text ID. Для legacy numeric/text-only-ID ссылки на прежние ID сохраняются буквально, но соответствующие цели под этими ID не восстанавливаются.

**Последствия:** исчезает прослеживаемая цепочка происхождения решения. Агент через broad recall получает формально валидные связи на отсутствующие записи; MCP не разрешает их и не сообщает, что конец цепочки потерян. История до обновлённого текста оказывается недоступна по прежней ссылке.

**Воспроизведение:** A=`43`, `superseded_by:["42"]`; B=`42`; update B. После успешного ответа `42` отсутствует, у A связь всё ещё `42`. Отдельно экспортировать legacy `42→43`, импортировать в пустое хранилище: оба факта есть под новыми ID, ссылка всё ещё `43`. Тесты: `TestReviewTextUpdateLeavesIncomingSupersessionDangling`, `TestReviewExportImportLegacyRelationshipsLoseTargets`.

**Попытка опровержения:** контракт прямо не обещает автоматическую взаимность отношений при **смене состояния** и допускает невалидированные цели. Здесь проверено разрушение уже существующей ссылки обычным текстовым update/round-trip, а не требование выводить новое отношение из семантики. Viz умеет показывать missing/cyclic/truncated history; это уменьшает ущерб для оператора, но не сохраняет граф и не добавляет такой сигнал в MCP recall. Для новых ID, не менявшихся после создания, export/import может сохранить ссылки — дефект round-trip условен legacy-ID.

**Минимальное исправление:** постоянный fact ID с отдельной версией текста; до этого — запрет или явная обработка update для входящих связей. Экспортировать ID и версию формата; импортировать с двухпроходной картой ID и проверкой ссылок.

### F10 — P2. Import смешивает duplicate, validation, failure и ambiguous write в одном успешном `skipped`

**Код:** [internal/memory/server.go:1230–1267,1286–1305,1313–1336,1368–1388](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:1230).

**Условия и цепочка:** embedding, exact lookup или upsert отдельного элемента не удался; либо запись применена, но ответ ошибочный. Элемент попадает в skipped, а инструмент в конце возвращает обычный `NewToolResultText` без `IsError`. Тот же счётчик используется для реального duplicate и validation rejection.

**Последствия:** агент не может определить, какие элементы существуют, какие пропущены намеренно, какие потеряны и какие могли записаться. В крайнем случае `Imported 0 facts, skipped 1.` возвращается при одной действительно записанной точке. При imported=0 также остаётся stale cache, описанный в F01.

**Воспроизведение:** импорт одного факта; mock выполняет upsert и затем отвечает 502. Tool result успешный: `Imported 0 facts, skipped 1.`, в хранилище одна запись. Тест: `TestReviewImportFailuresIndistinguishableFromSkipped`.

**Попытка опровержения:** ответ не утверждает, что весь импорт успешен, и это лучше безусловного «готово». Но различить пропуск и сбой из ответа невозможно; детали не возвращаются даже при нуле успешных подтверждений. Partial write не компенсируется транзакцией.

**Минимальное исправление:** структурированный per-item result с source index/ID и статусами `stored`, `duplicate`, `invalid`, `failed`, `ambiguous`; агрегированный partial/complete статус. Для ambiguous не рекомендовать слепой повтор.

### F11 — P2. Chunking теряет родительский заголовок, меняющий смысл найденного утверждения

**Код:** [internal/rag/chunker.go:22–27,66–92](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/chunker.go:22); [internal/rag/indexer.go:225–250](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/indexer.go:225); [internal/rag/server.go:169–175](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/server.go:169).

**Условия и цепочка:** раздел H1 содержит оговорку «историческое / больше не действует / гипотеза», а фактическое правило находится в H2. При каждом заголовке буфер сбрасывается, сохраняется только последний heading. Родительская оговорка не входит ни в text, ни в heading дочернего chunk и не участвует в его embedding. Поиск возвращает этот fragment независимо от родителя.

**Последствия:** из корректного документа извлекается правило без условия, которое делало его историческим или недействующим. Это особенно важно для endpoint/configuration и прежних решений: само содержание документа менять не нужно, чтобы агент получил вводящий в заблуждение фрагмент.

**Минимальное воспроизведение:**

```markdown
# Historical 2020 - no longer valid
## Endpoint
Use old.example
# Current
## Endpoint
Use new.example
```

Для old endpoint получается `heading="Endpoint"`, `text="## Endpoint\nUse old.example"`; квалификатора Historical нет. Тест: `TestReviewHeadingAncestorLost`.

**Попытка опровержения:** соседний H1 действительно индексируется отдельным chunk и иногда тоже может попасть в выдачу. Но sibling retrieval/parent expansion отсутствует, а top-k этого не гарантирует. `file_path` позволяет человеку найти источник вне MCP, но сам инструмент не предоставляет чтения документа/соседей по этому пути. Потеря qualifier подтверждена; конкретный выбор old chunk реальным embedding model здесь не измерялся.

**Минимальное исправление:** сохранять heading ancestry и включать её в representation каждого chunk; возвращать ancestor context/соседние блоки и ссылку на ревизию документа. Новое representation требует явного переиндексирования и проверки качества.

### F12 — P2. Некорректный HTTP-успешный envelope Qdrant превращается в пустой успешный результат

**Код:** [internal/qdrant/client.go:362–386,453–469,478–492,998–1005](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/qdrant/client.go:362).

**Условия и цепочка:** upstream вернул HTTP 200 с `{}`, `{"status":"error"}` или `{"result":null}`. `postJSON` не проверяет application status, а unmarshalling в struct с optional zero values не требует наличия result. Search создаёт пустой slice; ScrollAll считает обход завершённым.

**Последствия:** recall сообщает `No facts found.`, documents — `[]`, inventory — пусто. В write-preflight пустой search может разрешить запись. Агент не отличает повреждённый/не тот upstream-ответ от достоверного отсутствия данных.

**Воспроизведение:** mock endpoint поочерёдно возвращает три приведённых тела; Search и ScrollAll дают `err=nil`, ноль точек. Тест: `TestReviewMissingReadResultLooksEmpty` (три subtests).

**Попытка опровержения:** HTTP 4xx/5xx, transport error и malformed JSON уже приводят к ошибке. Не утверждается, что штатный Qdrant присылает эти нештатные envelopes; это подтверждённый дефект валидации при указанном upstream-сбое/подменённом endpoint, не наблюдённый production-инцидент. Mutation response проверяется существенно строже (`validateMutationResponse`).

**Минимальное исправление:** требовать правильный 2xx, валидный result нужного типа и согласованный application status; проверять обязательные поля точек и completion pagination. Только явный корректный пустой массив считать empty.

### F13 — P2. Повтор записи счётчика после неоднозначного исхода удваивает recall и влияет на operational selection

**Код:** [internal/memory/recall_counter.go:139–159](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/recall_counter.go:139); [internal/memory/server.go:1648–1675](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:1648); [internal/qdrant/client.go:548–555](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/qdrant/client.go:548).

**Условия и цепочка:** pending delta=1; worker читает count=0, отправляет SetPayload count=1; upstream применяет запись, но отдаёт ошибку. Delta остаётся pending. На следующем flush worker читает уже 1, прибавляет тот же delta и сохраняет 2.

**Последствия:** счётчик перестаёт означать число recalls, а основанный на нём выбор стартового контекста смещается к фактам, при обработке которых случались сбои. Повторные ambiguous failures могут накапливать завышение. Параллельные content upsert и несколько server instances дополнительно не защищены единым atomic increment; эти варианты отдельно не воспроизводились.

**Воспроизведение:** один pending event, первый SetPayload применён с ответом 502, следующий успешен; два вызова `flush` дают count=2, pending пуст. Тест: `TestReviewRecallCounterRetriesAppliedIncrement`.

**Попытка опровержения:** один worker предотвращает конкуренцию собственных increments, но не повторное применение после ошибки. Внешний запрет повторять `recall_facts` не влияет на внутренний retry worker. Best-effort при hard kill объясняет потерю событий, но не делает systematic duplication корректной метрикой для ranking.

**Минимальное исправление:** idempotency/event accounting или отдельный агрегатор; явно признать метрику приблизительной и не использовать её как свидетельство истинности. Не повторять read-plus-delta как безопасную идемпотентную запись.

### F14 — P3. Сводка папки включает скрытые документы, исключённые из корпуса chunks

**Код:** [internal/rag/summarizer.go:19–46](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/summarizer.go:19); [internal/rag/indexer.go:91–100](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/indexer.go:91).

**Условия и цепочка:** рядом с visible.md есть `.hidden.md`. Walk пропускает hidden file; folderSummary собирает все не-directory имена, а затем читает markdown без проверки leading dot. Его heading/snippet входят в embedding папки.

**Последствия:** первая стадия выбирает папку в том числе по тексту, который вторая стадия заведомо не может вернуть. Возникает дополнительная причина ложного routing. Подтверждено включение скрытого текста в summary; фактический сдвиг rank в реальном embedding space не измерялся. Утверждение об утечке текста через `search_documents` не делается.

**Воспроизведение:** visible.md и `.hidden.md` с уникальным `# Hidden policy`; `folderSummary` содержит Hidden policy. Тест: `TestReviewSummaryIncludesHiddenSource`.

**Попытка опровержения:** hidden-directory filtering и отдельная очистка legacy hidden-folder points есть; они не фильтруют hidden files внутри обычной папки при создании summary.

**Минимальное исправление:** единый predicate indexable-file для walk, summary и reconcile; после изменения обновить summary version.

## Подтверждённые ограничения и продуктовые компромиссы

Ниже поведение установлено по коду, но не объявляется нарушением уже заданного алгоритмического контракта. Риск принятия результата за полное доказательство остаётся.

| Класс | Что происходит и доказательство | Когда агент теряет контекст | Рекомендуемое изменение контракта |
|---|---|---|---|
| R1. Ограниченный fact candidate pool | `lifecycleCandidateLimit=max(20,4×limit)`, cap 400 (`lifecycle_adapter.go:37–47`); expiry/invalid фильтруются **после** Search (`server.go:847–855`). При limit=5 первые 20 expired скрывают валидный 21-й. `TestReviewRecallHandlerReturnsFalseEmptyAtCandidateLimit` проверяет настоящий handler: одна Search, затем `No facts found.`; после удаления synthetic expired head нужный факт виден. | Empty принимается за отсутствие знания во всём namespace. Canonical за границей пула не участвует вообще. | Не обещать исчерпывающий поиск; возвращать candidate budget, filtered count, saturated/incomplete. Перенести допустимые validity-фильтры в backend, добавить ограниченный refill. Сам лимит — компромисс; ложное обобщение empty — пробел presentation. |
| R2. Canonical без topic/entity scope | `lifecycle/model.go:419–454`, `lifecycle_recall.go:188–198`: любой canonical-current внутри пула выше любого ordinary-current, затем score. Нет minimum semantic threshold в recall. Тест `TestReviewBoundedPoolEmptyAndGlobalCanonical`: canonical с score .01 выше релевантного .99. | limit=1 может вытеснить релевантный факт canonical-записью на другую тему; обычный кандидат получает demote из-за чужого canonical. | Ввести область утверждения/сравнимости или ограничить authority reranking по relevance. Не трактовать `current_truth` как оценку истинности или score как вероятность. Policy сейчас именно так документирована. |
| R3. History/as_of — broad retrieval, не исторический snapshot | `lifecycle_recall.go:157–164,204–211`; в broad modes сохраняются current/disputed/historical/superseded с общими authority tiers. Не проверяются `created_at<=as_of` или временной интервал состояния. Истёкшее сегодня не видно даже в history/include_all. | На вопрос «что было решено тогда» актуальные записи могут заполнить limit и вытеснить прошлые; as_of может вернуть созданный позднее факт. | Назвать параметр expiry-reference либо явно вернуть `temporal_semantics:expiry_only` и отсутствие snapshot-гарантии; отдельный history state/time filter и limit policy. Это не неожиданная ошибка реализации: ограничение прямо присутствует в tool description и lifecycle contract. |
| R4. Hierarchical без rescue при слабом непустом hit | `rag/server.go:226–259`: top папок с threshold; внутри них chunks без threshold. Любой непустой ответ прекращает fallback. `TestReviewHierarchicalWeakHitPreventsFlatRescue` показывает score .01 в выбранной папке вместо доступного .99 в другой. | Нерелевантный body-hit принимается за лучший доступный документ; отсутствие по теме — за отсутствие в библиотеке. | В ответе явно указывать selected folders, actual mode, fallback и scope; рассмотреть bounded flat rescue. `flat` и `blended` уже доступны, но default намеренно hierarchical. Из этого ревью не следует, что blended всегда лучше. |
| R5. Противоречия и supersession требуют явного решения | `related.go:23–61`, `lifecycle/model.go:378–394`, `server.go:759–804,1065–1122`: cosine только выделяет duplicate/related; нет entailment, claim identity, автоматического reciprocal update или проверки существования replacement. | Два несовместимых current факта могут оба иметь `current_truth`; contested факт исчезает из current без сигнала о существующем споре; похожая формулировка с отрицанием может блокироваться dedup. | Явно представлять conflict/coverage как неизвестное; не выводить противоречие из similarity. Пара отрицаний с настоящим TEI и реальная частота false-dedup здесь **не проверялись**. |
| R6. Freshness — не свойство current | `RecallFact` не содержит created_at/updated_at/valid_until (`server.go:627–647`), optional verified_at может отсутствовать. В RAG stored indexed_at/file_hash не возвращаются. При update без lifecycle прежние provenance/verified_at/canonical копируются (`1010–1038`). | Агент не может определить возраст legacy/current факта или отличить старую проверку старого текста от подтверждения нового. | Возвращать источники, даты и verification scope/version; отсутствующее verified_at явно обозначать как not verified. Сохранение metadata при update заявлено контрактом, но это не доказательство свежести нового текста. |
| R7. Текстовый fallback беднее structuredContent | `server.go:1762–1786`: нет response mode/as_of/decision/reason_codes/point IDs/relationship IDs. State, canonical и provenance остаются. | Клиент, читающий только text, теряет intent и цепочку ID; не может выполнить надёжное exact-ID исправление по recall. | Обеспечить семантический паритет двух форматов, сохраняя понятную маркировку времени и uncertainty. Не считать наличие structured schema доказательством, что каждый клиент её использует. |
| R8. Индекс и согласованность между процессами | Auto-reindex по умолчанию 0; start не сообщает о готовности индекса (`rag/server.go:429–443`). Mutex reindex действует в одном Server; standalone indexer создаёт свой объект ([cmd/indexer/main.go:45–46](/Users/dzarlax/Projects/Code/Personal/personal_memory/cmd/indexer/main.go:45)). Mutation locks/cache тоже локальны процессу. | Старый индекс может жить без обновления; два писателя/индексатора не имеют общего snapshot/lock. | Явная single-writer deployment invariant, index status и lag; для нескольких процессов — координация. Конкуренция реальных процессов/Qdrant-кластера здесь не моделировалась, поэтому это ограничение гарантии, а не доказанный production race. |

Дополнительно: metadata модели проверяется до старта ([cmd/server/main.go:59–85](/Users/dzarlax/Projects/Code/Personal/personal_memory/cmd/server/main.go:59)), но не на каждом embedding-запросе. Замена TEI за тем же URL во время работы с другой моделью той же размерности — условный риск drift, не воспроизведённый инцидент. Pinning и startup guard снижают вероятность; periodic/connection-bound identity validation мог бы закрыть гарантию.

## Архитектура, восстановленная по актуальному коду

```text
MCP /memory + HTTP /memory/operational
  → auth / body limit
  → memory.Server
      reads: bounded TEI vector search → Qdrant filters
             → maintenance validation → lifecycle/expiry filtering
             → authority tiers → output limit → recall queue → cache / MCP
      writes: namespace stripe lock → exact checks + optional semantic preflight
              → Qdrant operation(s) → cache invalidation
      operational: ScrollAll → current/active/expiry
                   → all permanent + bounded recalled non-permanent

RAG на том же MCP server (ENABLE_RAG)
  query → TEI
        → hierarchical: folders → filtered chunks → flat only if empty
        → flat: chunks
        → blended: bounded folder + flat lists → RRF (+ optional injected reranker)
  output: score, text, relative file_path, heading, chunk_index
          + routing only for blended
  indexing: filesystem → per-file content hash → chunks + sequential upserts
             → delete prior generations → separate folder-summary reconcile

Qdrant collections: memory / configured chunks / configured folders
Embedding identity: model + revision + profile + dtype/pooling/dimension at startup
```

Основные точки сборки: [cmd/server/main.go:51–91,157–193](/Users/dzarlax/Projects/Code/Personal/personal_memory/cmd/server/main.go:51); factory memory — [internal/memory/server.go:151–160](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/memory/server.go:151); RAG — [internal/rag/server.go:81–95](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/rag/server.go:81); backend — [internal/qdrant/client.go:347–505](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/qdrant/client.go:347); batching TEI — [internal/embeddings/client.go:131–164](/Users/dzarlax/Projects/Code/Personal/personal_memory/internal/embeddings/client.go:131). Транспорт TEI/Qdrant имеет отдельные 30-секундные HTTP deadlines и ограниченное чтение response body. Общей транзакции между памятью, кэшем, счётчиками, chunks и folders нет.

**Что архитектура защищает и чем это подтверждается:**

- Startup identity guard сначала проверяет все коллекции, затем пишет metadata и повторно читает результат (`embeddingidentity/identity.go:131–227`). Существующие tests этого пакета выполнены. Это хорошая защита от детерминированного смешения vector spaces при старте, не гарантия качества retrieval.
- Malformed explicit lifecycle исключается из current-context; legacy без lifecycle остаётся current по явному правилу (`lifecycle/model.go:153–238`, `lifecycle_recall.go:177–179`). Disputed отдельно маркируется в broad mode (`lifecycle/presentation.go:45–88`). Тесты lifecycle/model/presentation и handler modes выполнены.
- Maintenance-active проверяется и backend filter, и локально (`maintenance_adapter.go:8–35`). Обычные read-paths не превращают quarantined в current. Тесты maintenance adapter выполнены; production quarantine/purge не проводились.
- Писатели одного Server сериализованы по namespace; update/delete повторно читают exact target под lock и проверяют namespace/maintenance; update отказывается перезаписывать конфликтующий newID (`server.go:48–74,983–1001,1143–1155`). Проверены существующие mutation-lock tests. Эти механизмы не сериализуют readers, другие процессы и счётчик.
- Mutations используют `wait=true` и проверяют completed-operation (`qdrant/client.go:865–970`); lifecycle-only batch задаёт strong ordering и ограничен lifecycle keys (`632–673`). Transport error не выдаётся за подтверждённый успех обычной мутации. Но несколько отдельных операций не образуют атомарную транзакцию.
- Cache key сериализован JSON, содержит namespace, query, отсортированные уникальные tags, limit и lifecycle intent/as_of; singleflight и generation guard защищают от типовых stale leader/collision ошибок (`server.go:887–925`, `cache.go:70–107`). Проверены прежние cache/singleflight tests. Найденная F01 относится к другой фазе операции.
- Indexer batch-embed выполняется до первого upsert файла и сохраняет старое поколение при embedding failure; обнаруживает неполные chunk indices; stale cleanup подавляется при неуспешном walk или подозрительно малом числе файлов (`rag/indexer.go:210–269,407–432,497–510`). Это защита сохранности старых данных, но не атомарной видимости и не полноты нового индекса.
- Обычные search dependency errors доходят до MCP `IsError`, а limit/mode validation выполняется до запроса (`memory/server.go:807–849`, `rag/server.go:140–163,186–210`). Не подтверждена гипотеза о безусловном превращении **любого** сбоя поиска в empty: F12 требует конкретного HTTP-успешного некорректного envelope.

**Архитектурная оценка:** локальные инварианты типизации, состояния, доступа и сохранности данных проработаны лучше, чем межоперационные гарантии. Главный разрыв — между «по отдельности корректные действия выполнены» и «читатель получил одну согласованную версию с известной полнотой». Lifecycle — классификация payload, не временной журнал и не truth engine. RAG — retrieval snippets из асинхронного индекса, не согласованное чтение источников. Контракт агента должен представлять именно эти гарантии.

## Рекомендуемый порядок исправлений

1. **Сначала запретить скрытое разрушение и stale success:** F02 exact-ID overwrite; F01 финальная/ambiguous invalidation. Добавить исходы частичных мутаций и контроль следующего чтения после подтверждённой записи.
2. **Сделать RAG-публикацию и её статус проверяемыми:** F03 committed generation; F04 aggregate errors/status; F05 durable folder reconciliation. Для чтения возвращать версию/дату и stale/partial, не ждать идеального алгоритма поиска.
3. **Унифицировать validity и write outcomes:** F06 renewal; F07 общая expiry policy; F08 import validation; F10 per-item outcomes; F12 strict read envelope. Проверять три независимых свойства: lifecycle valid, active, non-expired.
4. **Сохранить связь результата с источником:** F09 stable IDs/export graph, F11 heading ancestry; timestamps и parity structured/text из R6/R7. Изменения ID и representation проектировать как отдельные миграции с проверкой rollback.
5. **После этого менять качество ранжирования:** явно измерить R1–R5 на классах запросов, защитить identifier/path, temporal и negation cohorts. Наличие blended не повод автоматически менять default. Исправить F13 счётчики, F14 общие правила indexable-file.

Никакие исправления, миграции, reindex или изменения production в рамках этого ревью не запускались.

## Регрессионные сценарии по классам запросов

| Класс запроса / операции | Нужные synthetic варианты | Проверяемый результат |
|---|---|---|
| «Как сейчас настроен проект?» | current + superseded + disputed + expired + malformed + quarantined; cold/warm cache | Только operationally valid current; спор/неполнота не изображаются как доказанное отсутствие; срок/источник доступны. |
| Точное имя, путь, identifier | Ответ в 21-м/401-м кандидате; первые кандидаты expired/invalid; другой namespace; tags | Budget saturation виден; отсутствие после фильтра не выдаётся за полный отрицательный ответ; scope соблюдён. |
| Canonical и конфликт | Нерелевантный canonical; два разных topic canonical; два противоречивых current; точное отрицание с высоким cosine | Authority действует только в явной области; score не считается уверенностью; semantic dedup не подменяет разрешение конфликта. |
| «Что было тогда?» | Факт создан после as_of; transition после даты; history вытеснен current; expired historical | Явная expiry-only semantics до реализации истории; никаких заявлений о восстановленном snapshot. |
| Временная настройка | До/в/после valid_until, UTC-полночь, cache-hit через границу, malformed imported expiry | Одна включительность для recall/operational/related/stats; malformed не становится бессрочным; inventory обозначает expiry. |
| «Запомни это снова» | Exact-ID current/canonical, expired, superseded; одинаковый текст с новым expiry; сбои обоих searches | Existing metadata не стирается; duplicate/renewal/inconclusive явно различаются. |
| Исправление факта | A→B, текстовый update B; чтение между upsert/delete и после успеха; ошибка delete | Связи не исчезают; следующее чтение не содержит удалённого старого факта; partial mutation определима. |
| Импорт/экспорт | Numeric legacy IDs, supersession graph, invalid input, one-of-many fail, applied-but-error | Сохраняются identity/relationships; per-item stored/duplicate/invalid/ambiguous; повтор не создаёт незаметный overwrite. |
| Поиск регламента/инструкции | Historical H1 + Endpoint H2; оговорка в родителе; fenced code heading; несколько chunks одного файла | Возвращённый фрагмент сохраняет квалификаторы; версия/родители/соседи доступны. Fenced-code вариант пока только предложен. |
| Routing документов | Слабый hit в неверной папке, точный вне top folders, отсутствующая summary, hidden file | Actual scope/fallback видны; отсутствие summary самовосстанавливается; hidden не влияет на embedding summary. |
| Индексация при изменении | Сбой embedding, второго upsert, удаления old generation, folder upsert; второй Run без новых edits | Reader видит одно committed поколение; failures отражены; повтор исправляет и chunks, и folders. |
| Timeout/retry/конкуренция | Applied-but-error store/delete/counter, concurrent update/recall, canceled leader/follower, два процесса | Нет stale post-write success; ambiguous не equal failed/no-op; counters не дублируются; межпроцессные гарантии явно ограничены. |
| Пусто vs ошибка | Корректный `result:[]`; HTTP 502; malformed JSON; 200 `{}`/`result:null`; saturated filtered pool | Empty, failed и incomplete — разные исходы; агент может отличить их из tool result. |
| Стартовый operational context | Permanent expired/disputed, rarely recalled canonical, inflated recall_count, много permanent | Retention не превращается в authority; ограничение и критерий отбора обозначены; объём не маскируется как полный пользовательский контекст. |

## Выполненные проверки и ограничения

- Безусловный `make test` **не запускался**: он вызывает `dev-deps` и скачивает/перезаписывает browser assets. Вместо него выполнены выбранные Go-пакеты напрямую, с `GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`, временным GOCACHE. Версия Go: `go1.26.4 darwin/arm64`.
- Исходный прогон семи пакетов без дополнительных correctness probes прошёл: `internal/memory`, `memory/lifecycle`, `memory/maintenance`, `rag`, `qdrant`, `embeddings`, `embeddingidentity`.
- Окончательный `go test -race -count=1 -json` этих семи пакетов: **exit 0; 255 top-level tests, 494 test/subtest pass events; 22 дополнительных top-level probes; failures=0**. Стандартный go test также выполнил обычную встроенную vet-проверку при сборке тестов; отдельный полный `go vet ./...` не выполнялся.
- Новые probes специально утверждают наличие описанного поведения. Их PASS подтверждает воспроизведение, не исправление. Исходники probes и сетевой guard включены ниже, чтобы отчёт не зависел от сохранности временной папки. Helper-функции из существующих `_test.go` используются без изменения.
- По умолчанию среда запрещала `httptest` слушать loopback. После разрешённого запуска локальных тестов вне внешнего sandbox использован вложенный `sandbox-exec`, запрещающий сеть кроме loopback. В `TestMain` дополнительно установлен HTTP transport с запретом non-loopback destination. Окружение тестов очищено до PATH/HOME/TMPDIR/GOROOT/GOPATH и offline Go flags; реальные service URL/secrets не передавались. В проверенных тестах нет чтения production env или запуска внешних service commands. Не использовался даже уже работающий localhost Qdrant: mock servers создавали собственные ephemeral ports.
- Первые версии временного стенда потребовали исправления unused import, декодирования numeric Qdrant IDs и искусственного совпадения векторов разных import-facts. Эти ошибки тестового стенда не объявлены дефектами проекта. Все приведённые результаты относятся к окончательному прогону.
- Нет настоящего Qdrant/TEI integration replay, cosine-quality benchmark, проверки ANN recall, cluster replication/consistency, реальных данных, production logs, UI или живого поведения Codex/Claude/ChatGPT. Mock scoring управляемый; проверки shape/filter/ranking и обработчиков не доказывают, что ANN выдаст тот же набор в реальном корпусе.
- Midnight-cache проверен композиционно, не реальным ожиданием границы времени. Прогоны не являются исчерпывающим перебором schedules; отсутствие сообщений race detector не доказывает отсутствие логических гонок — F01 детерминированно существует при race-clean выполнении.
- OAuth/auth изучены лишь как сборка защищённых путей и граница доступа; это не security audit. Todoist, Viz UI, release/deploy, полноценная maintenance/migration recovery, CLI integration-bundle не проверялись end-to-end. Существующие unit tests maintenance выполнены; реальное обслуживание не запускалось.
- `conformance_adapter.go` генерирует artifact-policy traces, а не доказательство действий реального агента. Ранее сохранённые conformance/eval отчёты не засчитывались как свежие проверки.

Единственный постоянный артефакт этой работы — данный файл. Рабочие Go-исходники, зависимости, конфигурация, реальные данные и предшествующие изменения пользователя не редактировались. Commit/push/deploy не выполнялись. Дата имени файла сохранена по заданию; оформление отчёта завершалось после наступления 2026-09-05 в Europe/Belgrade.

## Приложение A. Начальный dirty state

```text
 M docs/ai-plans/2026-07-19-service-hardening-review-findings.html
 M docs/ai-plans/2026-07-20-embedding-model-identity.html
 M docs/ai-plans/2026-08-16-lifecycle-provenance-history-viz.html
?? .DS_Store
?? docs/.DS_Store
?? docs/ai-plans/2026-07-21-lifecycle-authority-semantics.html
?? docs/ai-plans/2026-07-22-related-fact-semantics.html
?? docs/ai-plans/2026-07-23-lifecycle-aware-mutations.html
?? docs/ai-plans/2026-08-01-cross-client-memory-usage-contract.html
?? docs/ai-plans/2026-08-01-retrieval-evaluation-harness.html
?? docs/ai-plans/2026-08-01-temporal-lifecycle-evaluation.html
?? docs/ai-plans/2026-08-02-model-memory-conformance-suite.html
?? docs/ai-plans/2026-08-02-query-document-embedding-hybrid-retrieval.html
?? docs/ai-plans/2026-08-03-hierarchical-document-routing-reranking.html
?? docs/ai-plans/2026-08-11-lifecycle-aware-recall-ranking.html
?? docs/ai-plans/2026-08-12-versioned-client-integration-bundle.html
?? docs/ai-plans/2026-08-14-lifecycle-aware-memory-maintenance.html
?? docs/ai-plans/2026-08-17-production-soak-release-gate.html
?? docs/ai-plans/2026-08-17-simple-installation-onboarding.html
?? docs/ai-plans/2026-08-20-issue-56-clean-machine-validation.html
?? docs/ai-plans/2026-08-29-gemini-manual-oauth-client.html
?? docs/superpowers/.DS_Store
?? docs/superpowers/plans/2026-08-28-lexical-retrieval-experiment.md
?? docs/superpowers/specs/2026-08-17-agent-assisted-setup-design.md
?? docs/superpowers/specs/2026-08-17-simple-bundle-installation-design.md
?? docs/superpowers/specs/2026-08-17-simple-local-installation-design.md
```

## Приложение B. Воспроизводимый локальный стенд

Ниже — только тестовые файлы. Вставлять их следует **в отдельную временную копию проверенного commit**, не в рабочее дерево и не в production. Код production-пакетов не меняется. Тесты используют существующие helper-функции из `_test.go` этого commit; mock не реализует ANN, транзакции или полный язык Qdrant filters. Для проверяемых путей ответы и сбои задаются детерминированно. В generation-тестах search handler получает точки, фактически оставшиеся после сбоя mock-индексатора.

Рецепт запуска на macOS с уже доступными Go-зависимостями:

```sh
review_dir=$(mktemp -d)
git archive 956e03cbf6e196a33a866db91b886d19cb94daae | tar -x -C "$review_dir"
# Создать нижеуказанные три review_correctness_test.go в review_dir.
# Добавить приведённый ниже TestMain в каждый проверяемый пакет,
# заменив PACKAGE на имя пакета (для memory/lifecycle — lifecycle и т.д.).
# Сохранить sandbox profile как "$review_dir/loopback.sb".
cd "$review_dir"
env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" \
  GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOCACHE="$review_dir/go-cache" \
  sandbox-exec -f "$review_dir/loopback.sb" \
  go test -race -count=1 -json \
  ./internal/memory/... ./internal/rag ./internal/qdrant \
  ./internal/embeddings ./internal/embeddingidentity
```

Sandbox profile:

```scheme
(version 1)
(allow default)
(deny network*)
(allow network* (local ip "localhost:*") (remote ip "localhost:*"))
```

Дополнительный transport guard, файл `review_network_guard_test.go` в каждом из семи пакетов. Он не нужен для функционального поведения; запрещает случайный non-loopback HTTP и отключает inherited proxy. В `memory/maintenance` и `embeddingidentity` тесты работают с in-memory doubles; guard установлен также там.

```go
package PACKAGE

import (
    "context"
    "fmt"
    "net"
    "net/http"
    "os"
    "testing"
)

func TestMain(m *testing.M) {
    http.DefaultTransport = &http.Transport{
        DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
            host, _, err := net.SplitHostPort(address)
            if err != nil { return nil, err }
            ip := net.ParseIP(host)
            if ip == nil || !ip.IsLoopback() {
                return nil, fmt.Errorf("review blocks non-loopback destination")
            }
            return (&net.Dialer{}).DialContext(ctx, network, address)
        },
    }
    os.Exit(m.Run())
}
```

Все нижеприведённые тесты ожидают **наблюдаемое проблемное поведение**. После исправления соответствующие assertions нужно инвертировать/заменить на целевой контракт; это не готовые acceptance tests исправленной версии.

<details>
<summary>internal/memory/review_correctness_test.go</summary>

```go
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Dzarlax-AI/personal-memory/internal/embeddings"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type reviewMemory struct {
	mu                                            sync.Mutex
	points                                        map[string]qdrant.Point
	srv                                           *Server
	deleteEntered, releaseDelete                  chan struct{}
	searchError, upsertAmbiguous, deleteAmbiguous bool
	searches                                      int
}

func newReviewMemory(t *testing.T) *reviewMemory {
	h := &reviewMemory{points: map[string]qdrant.Point{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embed" {
			var b struct {
				Inputs []string `json:"inputs"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			v := make([][]float32, len(b.Inputs))
			for i := range v {
				v[i] = reviewVector(b.Inputs[i])
			}
			json.NewEncoder(w).Encode(v)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/points/delete") && h.deleteEntered != nil {
			close(h.deleteEntered)
			<-h.releaseDelete
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			p, ok := h.points[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"result": p})
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			h.searches++
			if h.searchError {
				http.Error(w, "injected", 502)
				return
			}
			var b struct {
				Limit     int       `json:"limit"`
				Threshold *float64  `json:"score_threshold"`
				Vector    []float32 `json:"vector"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			ps := []qdrant.Point{}
			for _, p := range h.points {
				if b.Threshold != nil && len(b.Vector) > 0 && len(p.Vector) > 0 && b.Vector[0] != p.Vector[0] {
					p.Score = .8
				}
				if b.Threshold == nil || p.Score >= *b.Threshold {
					ps = append(ps, p)
				}
			}
			sort.Slice(ps, func(i, j int) bool {
				if ps[i].Score != ps[j].Score {
					return ps[i].Score > ps[j].Score
				}
				return ps[i].ID < ps[j].ID
			})
			if len(ps) > b.Limit {
				ps = ps[:b.Limit]
			}
			json.NewEncoder(w).Encode(map[string]any{"result": ps})
		case strings.HasSuffix(r.URL.Path, "/points/scroll"):
			ps := []qdrant.Point{}
			for _, p := range h.points {
				ps = append(ps, p)
			}
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"points": ps, "next_page_offset": nil}})
		case r.Method == http.MethodPut:
			var b struct {
				Points []qdrant.Point `json:"points"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			for _, p := range b.Points {
				p.Score = 1
				h.points[p.ID] = p
			}
			if h.upsertAmbiguous {
				http.Error(w, "applied but response failed", 502)
				return
			}
			fmt.Fprint(w, `{"result":{"status":"completed"}}`)
		case strings.HasSuffix(r.URL.Path, "/points/delete"):
			var b struct {
				Points []any `json:"points"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			for _, id := range b.Points {
				delete(h.points, fmt.Sprint(id))
			}
			if h.deleteAmbiguous {
				http.Error(w, "applied but response failed", 502)
				return
			}
			fmt.Fprint(w, `{"result":{"status":"completed"}}`)
		case strings.HasSuffix(r.URL.Path, "/points/payload"):
			var b struct {
				Points  []any          `json:"points"`
				Payload map[string]any `json:"payload"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			for _, rawID := range b.Points {
				id := fmt.Sprint(rawID)
				p, ok := h.points[id]
				if ok {
					for k, v := range b.Payload {
						p.Payload[k] = v
					}
					h.points[id] = p
				}
			}
			fmt.Fprint(w, `{"result":{"status":"completed"}}`)
		case strings.HasSuffix(r.URL.Path, "/points/batch"):
			fmt.Fprint(w, `{"result":[{"status":"completed"}]}`)
		default:
			http.Error(w, "unexpected route", 500)
		}
	}))
	h.srv = NewServer(qdrant.NewClient(server.URL, "memory"), embeddings.NewClient(server.URL), NewCache(time.Hour), "synthetic", .97, .60, .9)
	h.srv.recallCounter = newRecallCounter(context.Background(), h.srv.qdrant, 512, time.Hour)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		h.srv.Shutdown(ctx)
		server.Close()
	})
	return h
}
func (h *reviewMemory) seed(id, text string, extra map[string]any) {
	p := map[string]any{"text": text, "namespace": "projects", "recall_count": 0}
	for k, v := range extra {
		p[k] = v
	}
	h.points[id] = qdrant.Point{ID: id, Vector: reviewVector(text), Score: 1, Payload: p}
}
func reviewRecall(t *testing.T, h *reviewMemory) RecallFactsResult {
	t.Helper()
	r, e := h.srv.recallFacts(context.Background(), toolRequest(map[string]any{"query": "setting", "namespace": "projects"}))
	if e != nil || r.IsError {
		t.Fatalf("recall: %v %#v", e, r)
	}
	return r.StructuredContent.(RecallFactsResult)
}
func hasReviewText(r RecallFactsResult, text string) bool {
	for _, f := range r.Facts {
		if f.Text == text {
			return true
		}
	}
	return false
}

func TestReviewUpdateSuccessfulButCacheRetainsDeletedOldFact(t *testing.T) {
	h := newReviewMemory(t)
	h.seed("42", "old setting", nil)
	h.deleteEntered = make(chan struct{})
	h.releaseDelete = make(chan struct{})
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		r, _ := h.srv.updateFact(context.Background(), toolRequest(map[string]any{"point_id": "42", "new_fact": "new setting"}))
		done <- r
	}()
	<-h.deleteEntered
	during := reviewRecall(t, h)
	close(h.releaseDelete)
	result := <-done
	if result.IsError {
		t.Fatal(result)
	}
	h.mu.Lock()
	_, oldExists := h.points["42"]
	h.mu.Unlock()
	after := reviewRecall(t, h)
	if oldExists || !hasReviewText(during, "old setting") || !hasReviewText(after, "old setting") {
		t.Fatalf("defect not reproduced: oldExists=%v during=%+v after=%+v", oldExists, during, after)
	}
	t.Log("update succeeded and old ID absent; a later recall still returned old setting from cache")
}
func TestReviewAmbiguousDeleteLeavesCachedFact(t *testing.T) {
	h := newReviewMemory(t)
	h.seed("42", "old setting", nil)
	reviewRecall(t, h)
	h.deleteAmbiguous = true
	r, _ := h.srv.deleteFact(context.Background(), toolRequest(map[string]any{"point_id": "42"}))
	if !r.IsError {
		t.Fatal("want error")
	}
	after := reviewRecall(t, h)
	h.mu.Lock()
	_, exists := h.points["42"]
	h.mu.Unlock()
	if exists || !hasReviewText(after, "old setting") {
		t.Fatal("defect not reproduced")
	}
	t.Log("applied delete returned error; subsequent recall is stale success")
}
func TestReviewAmbiguousStoreLeavesCachedEmpty(t *testing.T) {
	h := newReviewMemory(t)
	if reviewRecall(t, h).Count != 0 {
		t.Fatal("want empty")
	}
	h.upsertAmbiguous = true
	r, _ := h.srv.storeFact(context.Background(), toolRequest(map[string]any{"fact": "new setting", "namespace": "projects"}))
	if !r.IsError {
		t.Fatal("want error")
	}
	if len(h.points) != 1 || reviewRecall(t, h).Count != 0 {
		t.Fatal("defect not reproduced")
	}
	t.Log("write applied but error preserved cached empty recall")
}
func TestReviewSupersededSameTextOverwritesHistoricalRecord(t *testing.T) {
	h := newReviewMemory(t)
	id := PointID("projects", "setting A")
	h.seed(id, "setting A", map[string]any{"lifecycle_state": "superseded", "superseded_by": []string{"42"}, "provenance": map[string]any{"source": "original"}})
	r, _ := h.srv.storeFact(context.Background(), toolRequest(map[string]any{"fact": "setting A", "namespace": "projects"}))
	if r.IsError {
		t.Fatal(r)
	}
	p := h.points[id]
	v := lifecycleView(id, p.Payload)
	if r.StructuredContent.(StoreFactResult).Status != "stored" || !v.Legacy || len(h.points) != 1 {
		t.Fatalf("defect not reproduced: %+v", v)
	}
	t.Log("same ID replaced superseded record with legacy current and discarded original provenance")
}
func TestReviewSearchFailureOverwritesExistingCanonicalFact(t *testing.T) {
	h := newReviewMemory(t)
	id := PointID("projects", "setting A")
	h.seed(id, "setting A", map[string]any{"lifecycle_state": "current", "canonical": true, "provenance": map[string]any{"source": "original"}})
	h.searchError = true
	r, _ := h.srv.storeFact(context.Background(), toolRequest(map[string]any{"fact": "setting A", "namespace": "projects"}))
	if r.IsError {
		t.Fatal(r)
	}
	v := lifecycleView(id, h.points[id].Payload)
	if !v.Legacy || v.Canonical {
		t.Fatal("defect not reproduced")
	}
	t.Log("both preflights failed; successful store silently overwrote canonical metadata")
}
func TestReviewExpiredDuplicateBlocksRenewalWithoutShowingExpiry(t *testing.T) {
	h := newReviewMemory(t)
	h.seed("42", "setting A", map[string]any{"valid_until": "2000-01-01"})
	r, _ := h.srv.storeFact(context.Background(), toolRequest(map[string]any{"fact": "setting A", "namespace": "projects", "valid_until": "2099-01-01"}))
	if r.IsError {
		t.Fatal(r)
	}
	v := r.StructuredContent.(StoreFactResult)
	b, _ := json.Marshal(v)
	if v.Status != "duplicate" || reviewRecall(t, h).Count != 0 || strings.Contains(string(b), "valid_until") {
		t.Fatalf("defect not reproduced: %s", b)
	}
	t.Log("renewal blocked; duplicate looks current, expiry is omitted, recall stays empty")
}
func TestReviewImportMalformedExpiryAcceptedAsCurrent(t *testing.T) {
	h := newReviewMemory(t)
	r, _ := h.srv.importFacts(context.Background(), toolRequest(map[string]any{"facts": `[{"text":"temporary setting","namespace":"projects","valid_until":"2000-99-99"}]`}))
	if r.IsError {
		t.Fatal(r)
	}
	if reviewRecall(t, h).Count != 1 {
		t.Fatal("defect not reproduced")
	}
	t.Log("invalid nonempty expiry imported and recalled as current_truth")
}
func TestReviewImportFailuresIndistinguishableFromSkipped(t *testing.T) {
	h := newReviewMemory(t)
	h.upsertAmbiguous = true
	r, _ := h.srv.importFacts(context.Background(), toolRequest(map[string]any{"facts": `[{"text":"setting","namespace":"projects"}]`}))
	text := toolResultText(t, r)
	if r.IsError || text != "Imported 0 facts, skipped 1." || len(h.points) != 1 {
		t.Fatalf("%#v points=%d", r, len(h.points))
	}
	t.Log("applied item reported as skipped in successful tool response")
}
func TestReviewExpiryDisagreesAndCacheCrossesDate(t *testing.T) {
	payload := map[string]any{"valid_until": time.Now().UTC().Format("2006-01-02")}
	if !isExpired(payload) || factExpiredAt(payload, time.Now()) {
		t.Fatal("expiry mismatch not reproduced")
	}
	p := qdrant.Point{ID: "42", Score: 1, Payload: map[string]any{"text": "setting", "valid_until": "2026-09-04"}}
	a := presentLifecycleRecallCandidates([]qdrant.Point{p}, LifecycleRecallOptions{}, time.Date(2026, 9, 4, 23, 59, 59, 0, time.UTC))
	b := presentLifecycleRecallCandidates([]qdrant.Point{p}, LifecycleRecallOptions{}, time.Date(2026, 9, 5, 0, 0, 1, 0, time.UTC))
	cache := NewCache(time.Hour)
	key := recallFactsCacheKey("q", "", nil, 5, LifecycleRecallOptions{})
	cache.SetRecall(key, RecallFactsResult{Facts: []RecallFact{{Text: "setting"}}})
	cached, ok := cache.GetRecall(key)
	if len(a) != 1 || len(b) != 0 || !ok || len(cached.Facts) != 1 {
		t.Fatal("defect not reproduced")
	}
	t.Log("expiry day differs across APIs; warm cache has no date dimension or expiry recheck (clock-boundary component test)")
}
func TestReviewBoundedPoolEmptyAndGlobalCanonical(t *testing.T) {
	ps := make([]qdrant.Point, 20)
	for i := range ps {
		ps[i] = qdrant.Point{ID: fmt.Sprint(i), Score: .99, Payload: map[string]any{"text": "expired", "valid_until": "2000-01-01"}}
	}
	if len(presentLifecycleRecallCandidates(ps, LifecycleRecallOptions{}, time.Now())) != 0 {
		t.Fatal("want filtered pool empty")
	}
	ps = append(ps, qdrant.Point{ID: "42", Score: .9, Payload: map[string]any{"text": "valid setting"}})
	if len(presentLifecycleRecallCandidates(ps, LifecycleRecallOptions{}, time.Now())) != 1 || lifecycleCandidateLimit(5) != 20 {
		t.Fatal("want relevant candidate beyond pool")
	}
	ranked := presentLifecycleRecallCandidates([]qdrant.Point{{ID: "1", Score: .99, Payload: map[string]any{"text": "relevant"}}, {ID: "2", Score: .01, Payload: map[string]any{"text": "unrelated", "lifecycle_state": "current", "canonical": true}}}, LifecycleRecallOptions{}, time.Now())
	if ranked[0].point.ID != "2" || ranked[0].Decision != LifecycleDecisionInclude {
		t.Fatal("want canonical promotion")
	}
	t.Log("bounded postfilter can erase valid tail; unrelated canonical outranks relevant current (documented ranking policy)")
}
func TestReviewTextUpdateLeavesIncomingSupersessionDangling(t *testing.T) {
	h := newReviewMemory(t)
	h.seed("42", "current setting", nil)
	h.seed("43", "historical setting", map[string]any{"lifecycle_state": "superseded", "superseded_by": []string{"42"}})
	r, _ := h.srv.updateFact(context.Background(), toolRequest(map[string]any{"point_id": "42", "new_fact": "corrected setting"}))
	if r.IsError {
		t.Fatal(r)
	}
	_, exists := h.points["42"]
	v := lifecycleView("43", h.points["43"].Payload)
	if exists || !v.Valid || v.SupersededBy[0] != "42" {
		t.Fatal("defect not reproduced")
	}
	t.Log("successful text update deletes referenced ID; referring lifecycle remains valid with dangling edge")
}

func TestReviewRecallCounterRetriesAppliedIncrement(t *testing.T) {
	count := 0
	writes := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "42", "payload": map[string]any{"recall_count": count}}})
			return
		}
		var b struct {
			Payload map[string]any `json:"payload"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		count = int(b.Payload["recall_count"].(float64))
		writes++
		if writes == 1 {
			http.Error(w, "applied but response lost", 502)
			return
		}
		fmt.Fprint(w, `{"result":{"status":"completed"}}`)
	}))
	defer s.Close()
	counter := recallCounter{qdrant: qdrant.NewClient(s.URL, "memory")}
	pending := map[string]int{"42": 1}
	counter.flush(context.Background(), pending)
	counter.flush(context.Background(), pending)
	if count != 2 || len(pending) != 0 {
		t.Fatalf("not reproduced: count=%d pending=%v", count, pending)
	}
	t.Log("one queued recall becomes two stored recalls after ambiguous write retry")
}
func TestReviewExportImportLegacyRelationshipsLoseTargets(t *testing.T) {
	old := newReviewMemory(t)
	old.seed("42", "old setting", map[string]any{"lifecycle_state": "superseded", "superseded_by": []string{"43"}})
	old.seed("43", "new setting", nil)
	exported, _ := old.srv.exportFacts(context.Background(), toolRequest(nil))
	fresh := newReviewMemory(t)
	imported, _ := fresh.srv.importFacts(context.Background(), toolRequest(map[string]any{"facts": toolResultText(t, exported)}))
	if imported.IsError {
		t.Fatal(imported)
	}
	v := lifecycleView(PointID("projects", "old setting"), fresh.points[PointID("projects", "old setting")].Payload)
	if len(fresh.points) != 2 || !v.Valid || v.SupersededBy[0] != "43" {
		t.Fatal("not reproduced")
	}
	if _, exists := fresh.points["43"]; exists {
		t.Fatal("legacy id preserved")
	}
	t.Log("export/import preserved edge literal but discarded legacy target IDs")
}
func TestReviewListExpiredFactWithoutExpiryWarning(t *testing.T) {
	h := newReviewMemory(t)
	h.seed("42", "expired setting", map[string]any{"valid_until": "2000-01-01"})
	r, _ := h.srv.listFacts(context.Background(), toolRequest(nil))
	text := toolResultText(t, r)
	if r.IsError || !strings.Contains(text, "state:current") || strings.Contains(text, "2000-01-01") || strings.Contains(text, "valid_until") {
		t.Fatal("not reproduced")
	}
	t.Log("inventory emits expired entry as state:current legacy without expiration metadata")
}

func TestReviewRecallHandlerReturnsFalseEmptyAtCandidateLimit(t *testing.T) {
	h := newReviewMemory(t)
	for i := 0; i < 20; i++ {
		h.seed(fmt.Sprint(i+100), "expired", map[string]any{"valid_until": "2000-01-01"})
	}
	h.seed("42", "valid setting", nil)
	p := h.points["42"]
	p.Score = .9
	h.points["42"] = p
	r := reviewRecall(t, h)
	if r.Count != 0 || h.searches != 1 {
		t.Fatalf("not reproduced: %+v", r)
	}
	h.srv.cache.Invalidate()
	h.mu.Lock()
	for id, p := range h.points {
		if p.Payload["text"] == "expired" {
			delete(h.points, id)
		}
	}
	h.mu.Unlock()
	if !hasReviewText(reviewRecall(t, h), "valid setting") {
		t.Fatal("valid fact is not retrievable")
	}
	t.Log("actual recall handler returned No facts found after filtering saturated pool; lower-ranked valid fact exists")
}

func reviewVector(text string) []float32 {
	n := 0
	for i, c := range text {
		n += (i + 1) * int(c)
	}
	return []float32{float32(n), 1}
}
```

</details>

<details>
<summary>internal/rag/review_correctness_test.go</summary>

```go
package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Dzarlax-AI/personal-memory/internal/config"
	"github.com/Dzarlax-AI/personal-memory/internal/qdrant"
	"github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewPartialGenerationReturnedWithoutVersion(t *testing.T) {
	h := newGenerationHarness(t, 20)
	path := writeRAGFile(t, h.idx.docsDir, strings.Repeat("new setting. ", 10))
	h.seed(path, "old-generation", 1)
	h.failUpsertAt = 2
	changed, e := h.idx.indexFile(context.Background(), path, h.state(t, path))
	if !changed || e == nil {
		t.Fatal("want partial failure")
	}
	ps := []qdrant.Point{}
	generations := map[string]bool{}
	for _, p := range h.points {
		ps = append(ps, p)
		generations[p.Payload["generation"].(string)] = true
	}
	srv := &Server{queryEmbed: fakeQueryEmbedder{}, searchChunks: fakePointSearcher{search: func(map[string]any) []qdrant.Point { return ps }}, cfg: &config.Config{RAGDocumentsDir: h.idx.docsDir}}
	result, err := srv.handleSearchDocuments(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"query": "setting", "mode": "flat"}}})
	text := ragToolResultText(t, result)
	if err != nil || result.IsError || len(generations) != 2 || strings.Contains(text, "generation") || strings.Contains(text, "indexed_at") {
		t.Fatalf("defect not reproduced: %s %v", text, err)
	}
	t.Log("old complete + new partial generation are exposed together, without generation or freshness fields")
}
func TestReviewRunSuppressesIndexError(t *testing.T) {
	h := newGenerationHarness(t, 100)
	writeRAGFile(t, h.idx.docsDir, "new setting")
	h.failEmbed = true
	if e := h.idx.Run(context.Background()); e != nil {
		t.Fatalf("defect not reproduced: %v", e)
	}
	if len(h.points) != 0 {
		t.Fatal("want empty")
	}
	t.Log("embedding failed, nothing indexed, Run returned nil")
}
func TestReviewFolderFailureNotRetried(t *testing.T) {
	h := newGenerationHarness(t, 100)
	path := writeRAGFile(t, h.idx.docsDir, "# Current topic\nbody")
	failFolder := true
	folderAttempts := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/folders/") && strings.HasSuffix(r.URL.Path, "/scroll") {
			ps := []qdrant.Point{}
			for _, p := range h.points {
				if _, ok := p.Payload["summary"]; ok {
					ps = append(ps, p)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"points": ps, "next_page_offset": nil}})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/collections/folders/points" {
			folderAttempts++
			if failFolder {
				http.Error(w, "folder write failed", 502)
				return
			}
		}
		h.serveHTTP(w, r)
	}))
	defer proxy.Close()
	h.idx.folders = qdrant.NewClient(proxy.URL, "folders")
	if e := h.idx.Run(context.Background()); e == nil {
		t.Fatal("want folder error")
	}
	failFolder = false
	if e := h.idx.Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	if folderAttempts != 1 {
		t.Fatalf("folder unexpectedly retried: %d", folderAttempts)
	}
	if _, ok := h.points[folderPointID(filepath.Dir(path))]; ok {
		t.Fatal("want absent folder")
	}
	t.Log("second successful Run skipped unchanged chunks and never recreated missing summary")
}
func TestReviewHierarchicalWeakHitPreventsFlatRescue(t *testing.T) {
	n := 0
	srv := &Server{queryEmbed: fakeQueryEmbedder{}, searchFolders: fakePointSearcher{search: func(map[string]any) []qdrant.Point {
		return []qdrant.Point{{ID: "f", Score: .8, Payload: map[string]any{"folder_path": "/docs/wrong"}}}
	}}, searchChunks: fakePointSearcher{search: func(filter map[string]any) []qdrant.Point {
		n++
		if filter != nil {
			return []qdrant.Point{{ID: "weak", Score: .01, Payload: map[string]any{"text": "unrelated", "file_path": "/docs/wrong/a.md"}}}
		}
		return []qdrant.Point{{ID: "right", Score: .99, Payload: map[string]any{"text": "answer", "file_path": "/docs/right/b.md"}}}
	}}, cfg: &config.Config{RAGDocumentsDir: "/docs", RAGFolderTopK: 3, RAGFolderThreshold: .5}}
	r, _ := srv.handleSearchDocuments(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"query": "answer"}}})
	text := ragToolResultText(t, r)
	if r.IsError || n != 1 || !strings.Contains(text, "unrelated") || strings.Contains(text, "routing") {
		t.Fatal("defect not reproduced")
	}
	t.Log("weak nonempty folder hit suppresses flat search, scope omitted (documented routing tradeoff)")
}
func TestReviewHeadingAncestorLost(t *testing.T) {
	cs := chunkMarkdown("# Historical 2020 - no longer valid\n## Endpoint\nUse old.example\n# Current\n## Endpoint\nUse new.example", 1500)
	for _, c := range cs {
		if strings.Contains(c.text, "old.example") {
			if strings.Contains(c.text, "Historical") || strings.Contains(c.heading, "Historical") {
				t.Fatal("ancestor unexpectedly preserved")
			}
			t.Logf("historical qualification absent from returned chunk: heading=%q text=%q", c.heading, c.text)
			return
		}
	}
	t.Fatal("old endpoint not found")
}
func TestReviewSummaryIncludesHiddenSource(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "visible.md"), []byte("# Visible\nbody"), 0600)
	os.WriteFile(filepath.Join(d, ".hidden.md"), []byte("# Hidden policy\nNever use current endpoint"), 0600)
	s, e := folderSummary(d, "")
	if e != nil || !strings.Contains(s, "Hidden policy") {
		t.Fatalf("not reproduced: %s %v", s, e)
	}
	fmt.Sprint(s)
	t.Log("folder routing input includes hidden document excluded from chunk walk")
}
```

</details>

<details>
<summary>internal/qdrant/review_correctness_test.go</summary>

```go
package qdrant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReviewMissingReadResultLooksEmpty(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"error"}`, `{"result":null}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer s.Close()
			c := NewClient(s.URL, "synthetic")
			p, e := c.Search(context.Background(), []float32{1, 1}, 5, nil, nil)
			if e != nil || len(p) != 0 {
				t.Fatal("search defect not reproduced")
			}
			ps, e := c.ScrollAll(context.Background(), nil, false)
			if e != nil || len(ps) != 0 {
				t.Fatal("scroll defect not reproduced")
			}
			t.Log("HTTP 200 with invalid result envelope accepted as empty success")
		})
	}
}
```

</details>

## Приложение C. Результат окончательного прогона

```json
{
  "exit": 0,
  "test_and_subtest_passes": 494,
  "top_level_tests": 255,
  "review_top_level_tests": 22,
  "failed": []
}
```

Финальная сверка: ветка и commit прежние; Go-исходники и manifests зависимостей побайтно совпадают с тестовой копией commit. По сравнению с исходным porcelain-status добавлена только строка `?? docs/reviews/2026-09-04-personal-memory-correctness-review.md`, исчезнувших строк нет.
