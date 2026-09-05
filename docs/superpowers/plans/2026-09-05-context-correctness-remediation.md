# Context Correctness Remediation Implementation Plan

> **Status:** Approved and implemented on 2026-09-05. This is now the execution record; its approval language is retained only as historical context.

> **Execution:** Choose inline or delegated execution based on scope and explicit authorization. Minimize Sol usage; assign implementation to Luna/Terra and justify every Sol exception. Steps use checkbox (`- [ ]`) syntax when tracking is useful.

**Goal:** Remove the confirmed paths that return stale, silently overwritten, mixed-generation, or falsely empty context, while preserving the documented lifecycle and availability contracts.

**Architecture:** Treat fact writes as an explicit state-transition boundary: exact IDs cannot be silently reused, every dispatched visible mutation invalidates cached reads, and mutation outcomes expose ambiguity instead of presenting it as absence. Treat document generations as unpublished until a reader-visible publication invariant has been selected and proven; do not claim atomicity from sequential Qdrant upserts.

**Tech Stack:** Go 1.26, mcp-go, Chi, Qdrant REST, TEI, existing lifecycle contract and Go test suite.

---

## Scope and non-goals

This plan implements the confirmed defects F01–F14 from `docs/reviews/2026-09-04-personal-memory-correctness-review.md`, in three independently releasable waves. It does not change the documented product tradeoffs R1–R8 (canonical topic scoping, full historical snapshots, default hierarchical routing, semantic contradiction detection, or a relevance-confidence score). Those require product-policy decisions and fresh evaluation evidence.

The original implementation approval excluded deployment, production-data migration, and production access. A commit and PR were created later under separate explicit user authorization. A production data migration remains a separately approved task after the new reader/writer compatibility contract is implemented and tested.

## File map

| Area | Files | Responsibility |
|---|---|---|
| Fact mutation and read contract | `internal/memory/server.go`, `internal/memory/id.go`, `internal/memory/related.go`, `internal/memory/lifecycle_recall.go`, `internal/memory/recall_counter.go` | Exact-ID collision policy, cache invalidation, expiry semantics, bounded-read disclosure, counter ambiguity. |
| Fact tests and public contract | `internal/memory/*_test.go`, `website/src/content/docs/lifecycle/fact-lifecycle-contract.md` | Controlled interleavings and user-visible result semantics. |
| Qdrant boundary | `internal/qdrant/client.go`, `internal/qdrant/client_test.go` | Strict read-envelope validation and only operations needed by the chosen document-publication design. |
| RAG index and reader | `internal/rag/indexer.go`, `internal/rag/server.go`, `internal/rag/summarizer.go`, `internal/rag/*_test.go` | Publication state, durable folder reconciliation, file-error reporting, hidden-file parity. |
| Operators and evaluation | `cmd/indexer/main.go`, `cmd/server/main.go`, `internal/eval/*`, `evaldata/public/v*/` only if fixtures need new deterministic assertions | Observable reindex completion and regressions without changing retrieval-quality baselines. |

## Model-led execution strategy

| Workstream | Primary model | Effort | Owned files | Required checks | Coordinator gate |
|---|---|---:|---|---|---|
| Freeze mutation/read result contract | Terra | high | `internal/memory/server.go`, lifecycle docs, memory tests | focused memory tests and schema inspection | exact outcome compatibility |
| Fact correctness implementation | Terra | high | memory package files and tests | race tests with controlled HTTP failures | review of cache and collision invariants |
| Qdrant envelope validation | Luna | medium | `internal/qdrant/client.go`, tests | `go test ./internal/qdrant -count=1` | no broadened transport semantics |
| RAG recovery and summary parity | Terra | high | `internal/rag/*`, `cmd/indexer/*` | focused RAG tests | reindex status and recovery proof |
| Documentation and regression-fixture updates | Luna | medium | contract docs and deterministic fixtures | documentation links and targeted tests | no production corpus access |
| Publication-invariant decision | Astra | xhigh | design decision only; no implementation files | adversarial interleaving design review | required before RAG generation implementation |

At least 90% of implementation is owned by Terra/Luna. Astra is an exception-only gate: Qdrant point operations are sequential, while F03 requires a reader-visible multi-point publication invariant. Terra must first supply two viable designs and tests; Astra selects or rejects the guarantee before code is written. Astra owns no implementation lane.

### Execution waves

1. **Contract freeze — Terra high:** freeze result shapes and compatibility behavior for exact collisions, ambiguous writes, expiry and saturated reads. This unlocks Wave 2.
2. **Independent fact lanes — Terra/Luna:** implement F01/F02/F06–F10/F12–F13 and strict Qdrant envelopes with disjoint test ownership. F11 belongs solely to Wave 4 because it is a RAG publication/recovery invariant. Completion requires all targeted race and unit tests.
3. **Astra publication gate:** select and record the RAG active-generation invariant. This is blocked until Wave 2 has established the shared failure/result conventions.
4. **RAG recovery — Terra high and Luna medium:** implement F03–F05/F11/F14 against the selected invariant; provide status and retry semantics.
5. **Integration — Terra high:** run broad offline checks, inspect contract/document diffs, and update the approval artifact with actual results. Production or release actions remain out of scope.

## Frozen compatibility decisions

1. Existing valid `current`, `historical`, `superseded`, and `disputed` payloads remain readable. No lifecycle state is inferred from similarity.
2. A point whose deterministic ID already exists must never be overwritten by `store_fact` or `import_facts`. The response is a distinct exact-collision outcome, carrying the existing point ID and lifecycle summary; reactivation remains `set_fact_lifecycle`.
3. `valid_until` accepts only `YYYY-MM-DD`, means valid through that UTC calendar date everywhere, and is included in inventory/duplicate results where expiry affects the caller's next action.
4. A dispatched mutation invalidates cache regardless of response outcome. A successful `update_fact` invalidates after its last visibility-changing request; an unsuccessful delete reports a partial/ambiguous mutation, never a clean update.
5. A bounded read that fills its backend candidate budget but yields fewer eligible results is explicitly incomplete. It must not be formatted as unqualified `No facts found.`
6. `import_facts` returns per-item outcome counts and item indexes for `stored`, `duplicate`, `invalid`, `dependency_failed`, `inconclusive`, and `ambiguous`; it must not turn all non-stored items into `skipped`.
7. The recall counter becomes best-effort telemetry only until Qdrant offers an idempotent/atomic increment primitive. After an ambiguous `SetPayload`, discard that event with a metric/log rather than retrying the same delta.

## Task 1: Freeze the fact mutation and read-result contract

**Execution owner:** Terra, high.
**Coordinator gate:** The response structs and text fallbacks must preserve old successful results while adding explicit non-success states.

**Files:**

- Modify: `internal/memory/server.go`, `internal/memory/related.go`, `internal/memory/lifecycle_recall.go`.
- Modify: `internal/memory/related_response_test.go`, `internal/memory/lifecycle_integration_test.go`, `internal/memory/server_test.go`.
- Modify: `website/src/content/docs/lifecycle/fact-lifecycle-contract.md`.

- [ ] Define explicit result values before changing handlers. The public types must distinguish exact collision, exhausted candidate window, and mutation ambiguity:

```go
type MutationOutcome string

const (
    MutationStored        MutationOutcome = "stored"
    MutationDuplicate     MutationOutcome = "duplicate"
    MutationExactCollision MutationOutcome = "exact_collision"
    MutationPartial       MutationOutcome = "partial"
    MutationAmbiguous     MutationOutcome = "ambiguous"
)

type RecallCoverage struct {
    CandidateLimit int  `json:"candidate_limit"`
    CandidateCount int  `json:"candidate_count"`
    EligibleCount  int  `json:"eligible_count"`
    Incomplete     bool `json:"incomplete"`
}
```

- [ ] Add failing tests for: an existing active exact ID with unavailable semantic preflight; an existing superseded exact ID; 20 expired candidates before a valid candidate; a duplicate blocked by expiry where the response contains `valid_until`; and a malformed imported expiry. Each test must assert the structured value and text fallback separately.

- [ ] Implement one shared exact-point lookup used by both `storeFact` and each `importFacts` candidate. It must inspect lifecycle/maintenance state only after establishing existence, and return `MutationExactCollision` without calling `Upsert`.

- [ ] Make lifecycle-aware candidate presentation return coverage alongside facts. Preserve normal empty behavior only when `Incomplete == false`; otherwise return a structured incomplete response whose text says that the bounded candidate window was exhausted.

- [ ] Replace all ad-hoc expiry parsing in memory read, duplicate, inventory, import validation, and eval helpers with a single UTC-calendar parser owned by `internal/memory/lifecycle` or a narrow shared internal package. Reject non-`YYYY-MM-DD` input before a write.

Run:

```sh
GOCACHE=/private/tmp/personal-memory-go-build go test ./internal/memory -count=1
GOCACHE=/private/tmp/personal-memory-go-build go test ./internal/eval -count=1
```

Expected: PASS; new tests demonstrate explicit collision/incomplete/expiry outcomes and existing lifecycle compatibility tests remain green.

## Task 2: Make fact cache and mutation outcomes safe after dispatch

**Execution owner:** Terra, high.
**Coordinator gate:** A recall begun after successful update completion cannot receive the deleted ID from cache.

**Files:**

- Modify: `internal/memory/server.go`, `internal/memory/cache.go`, `internal/memory/recall_counter.go`.
- Modify: `internal/memory/server_test.go`, `internal/memory/cache_test.go`, `internal/memory/recall_counter_test.go`.

- [ ] Add controlled HTTP-handler interleaving tests for the three cases below; use channels, not sleeps:

```go
// 1. Hold Delete(oldID), issue recall after the first invalidation,
//    release Delete, await update success, then assert a new backend search
//    and no oldID in the second recall.
// 2. Make Upsert/Delete apply but return 502; assert cache is invalidated
//    and result is MutationAmbiguous/MutationPartial.
// 3. Make SetPayload apply but return 502; assert the pending recall delta
//    is discarded rather than retried.
```

- [ ] Refactor `updateFact` so it records whether each visibility-changing request was dispatched. Call cache invalidation in a deferred finalizer whenever an upsert/delete dispatch occurred, and perform an additional invalidation after successful old-ID deletion. Return structured mutation detail instead of only `Updated: ...` when a two-step update is partial.

- [ ] Apply the same dispatched-write invalidation rule to `storeFact`, `deleteFact`, and import batches. Do not convert ambiguous transport failure to success.

- [ ] In `recallCounter.flush`, remove a pending delta after a `SetPayload` error and emit a content-free warning/metric. Do not retry a read-modify-write increment whose prior application cannot be known.

Run:

```sh
GOCACHE=/private/tmp/personal-memory-go-build go test -race ./internal/memory -run 'Test(Update|Recall|Store|Delete|RecallCounter)' -count=1
GOCACHE=/private/tmp/personal-memory-go-build go test ./internal/memory -count=1
```

Expected: PASS; the update interleaving requires a second backend search after completion, and ambiguous increments never become two stored increments.

## Task 3: Tighten the Qdrant client boundary

**Execution owner:** Luna, medium.
**Coordinator gate:** A 2xx response missing a valid `result` envelope is a client error, never a zero-row success.

**Files:**

- Modify: `internal/qdrant/client.go`.
- Modify: `internal/qdrant/client_test.go`.

- [ ] Define a decoder used by `Search`, `ScrollAll`, `Get`, and collection reads that rejects missing `result`, `result:null`, and an error status represented in a 2xx body. Keep legitimate empty arrays and empty scroll pages successful.

```go
func decodeRequiredResult(body io.Reader, target any) error {
    var envelope struct {
        Status string          `json:"status"`
        Result json.RawMessage `json:"result"`
    }
    if err := json.NewDecoder(body).Decode(&envelope); err != nil { return err }
    if envelope.Status == "error" || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
        return errors.New("qdrant response has no usable result")
    }
    return json.Unmarshal(envelope.Result, target)
}
```

- [ ] Test `{}`, `{"status":"error"}`, and `{"result":null}` for search and scroll; test `{"result":[]}` and an empty valid scroll page as success.

Run:

```sh
GOCACHE=/private/tmp/personal-memory-go-build go test ./internal/qdrant -count=1
```

Expected: PASS; only structurally valid Qdrant empty results are accepted.

## Task 4: Select and prove the RAG generation-publication invariant

**Execution owner:** Terra, high design brief; Astra, xhigh decision gate.
**Coordinator gate:** Astra must approve one invariant and its adversarial tests before implementation begins.

**Files:**

- Design inputs: `internal/rag/indexer.go`, `internal/rag/server.go`, `internal/qdrant/client.go`, `internal/rag/indexer_test.go`, `internal/rag/server_test.go`.
- Decision record: `docs/ai-plans/2026-09-05-context-correctness-remediation.html` (update its decision section after approval).

- [ ] Terra produces two concrete designs with Qdrant request sequences and compatibility impact:

  1. A version manifest per file plus reader-side resolution that never returns a generation unless all expected chunks are marked committed.
  2. A separate immutable published-file index that reader queries use to validate/deduplicate chunks by `(file_path, generation)` before formatting results.

- [ ] For both designs, prove these scenarios with controlled search results: old complete + new partial; all new chunks written before commit; commit response lost; old-generation cleanup fails; standalone indexer overlaps the server reindex request. The reader must output one complete generation per file or return explicit incompleteness; it may not silently mix versions.

- [ ] Astra selects only a design that can state its query-time invariant without a cross-point atomicity assumption. If neither design does, stop and return a narrowed design decision rather than implementing a false atomic claim.

Required decision artifact:

```text
Invariant: every returned (file_path, generation) has total_chunks distinct
chunk_index values and a durable committed marker observed by this reader.
Failure result: no complete committed generation is available for file_path;
the response marks that file omitted/incomplete rather than returning partial text.
```

## Task 5: Implement RAG publication, recovery, and observable reindex status

**Execution owner:** Terra, high; Luna, medium for tests/documentation.
**Coordinator gate:** The selected Task 4 invariant is implemented exactly; no hidden fallback switches it off.

**Files:**

- Modify: `internal/rag/indexer.go`, `internal/rag/server.go`, `internal/rag/summarizer.go`.
- Modify: `internal/rag/indexer_test.go`, `internal/rag/server_test.go`, `internal/rag/summarizer_test.go`.
- Modify: `cmd/indexer/main.go`, `website/src/content/docs/lifecycle/fact-lifecycle-contract.md` if RAG response fields become public.

- [ ] Implement Task 4's committed-generation marker and reader filtering. Do not delete an older complete generation until the new one is durably committed; cleanup failure must preserve a readable committed version.

- [ ] Aggregate every file read/embed/upsert failure in `Indexer.Run`. Return an error when any file could not be indexed, while retaining guarded stale cleanup. Include non-sensitive counts in the result: visited, unchanged, indexed, failed, folders refreshed, folders failed.

- [ ] Persist a retryable folder-dirty condition when `reconcileFolder` fails. On the next run, refresh that folder even when all chunks are unchanged; clear the condition only after a successful folder upsert or guarded deletion.

- [ ] Make `folderSummary` skip hidden files using the same predicate as the document walk. Add a test with `visible.md` and `.hidden.md` that asserts neither the filename nor its heading/snippet affects the summary.

- [ ] Change `reindex_documents` from start-only text to a request ID plus an observable terminal status endpoint/tool. A start acknowledgement is not a completed reindex result.

Run:

```sh
GOCACHE=/private/tmp/personal-memory-go-build go test ./internal/rag -count=1
GOCACHE=/private/tmp/personal-memory-go-build go test -race ./internal/rag -run 'Test(Index|Search|Reindex|Folder)' -count=1
GOCACHE=/private/tmp/personal-memory-go-build go test ./cmd/indexer -count=1
```

Expected: PASS; every failure injection yields an error or explicit terminal partial status, and search never formats mixed generations as a normal answer.

## Task 6: Integrate, document, and verify offline

**Execution owner:** Terra, high.
**Coordinator gate:** Inspect the actual diff and prove every report finding has either a regression test or a documented, accepted non-goal.

**Files:**

- Modify: `docs/reviews/2026-09-04-personal-memory-correctness-review.md` only to cross-link implemented regression coverage.
- Modify: `docs/ai-plans/2026-09-05-context-correctness-remediation.html` with actual changes and evidence after implementation.

- [ ] Build a finding-to-test matrix for F01–F14. Each row records exact test name, behavior asserted, and whether it is a unit, controlled-handler, or Qdrant/TEI integration test.
- [ ] Run the project checks that do not require production secrets or a live corpus:

```sh
GOCACHE=/private/tmp/personal-memory-go-build go test ./internal/memory ./internal/rag ./internal/qdrant ./internal/eval ./internal/retrieval ./integrationbundle -count=1
make eval-public
git diff --check
```

- [ ] Run a local isolated Qdrant/TEI integration only if its Compose/configuration is inspected first and it has no access to production collections. Verify all new response fields with an MCP client fixture.
- [ ] Do not commit, push, deploy, or run lifecycle/data migration. Present the diff, check output, compatibility limits, and a separate migration/deployment recommendation for approval.

## Risks, rollback, and approval criteria

**Risks:** changing deterministic-ID collision behavior can break clients that treated `store_fact` as an update; adding public fields needs text/structured parity; candidate refill can affect latency; a RAG publication layer can increase Qdrant reads; counter events may be lost deliberately after ambiguous writes. These are explicit safety/availability tradeoffs, not silent behavior changes.

**Rollback:** ship compatibility readers before writers; guard newly written generation metadata behind a reader that still understands legacy chunks; revert new writes first if integration fails. Do not delete old points, generations, or lifecycle metadata as part of rollback. A later migration must be snapshot-backed and separately approved.

**Approval criteria:** approve only if (1) exact collision and ambiguous-write result schemas are accepted, (2) the RAG invariant selected in Task 4 is acceptable, (3) explicit incomplete responses are acceptable for saturated reads/reindex partial failure, and (4) no deployment or data migration is included.

## Implementation record — 2026-09-05

The approval criteria above were accepted before implementation. The delivered work includes explicit fact outcomes, UTC expiry alignment, strict Qdrant envelopes, dispatched-write cache invalidation, and sealed immutable RAG generations. The PR review follow-up additionally rejects malformed point IDs and out-of-root candidates, restores the public `search_documents` contract to `hierarchical|flat`, rejects documents above the generation-validation cap before embedding or writing, and never rebinds a stale generation's score to newer content. Existing unsealed RAG chunks remain explicitly omitted as `legacy_unverified`; no migration, production reindex, deployment, or legacy backfill was performed.
