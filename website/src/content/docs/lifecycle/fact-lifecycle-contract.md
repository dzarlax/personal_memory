---
title: Fact lifecycle contract
---

# Fact Lifecycle Contract

This document defines how Personal Memory classifies stored facts as current context or inspectable history. The contract is additive: existing facts remain readable and are treated as current until lifecycle metadata is explicitly added.

## States

Every fact with valid lifecycle metadata has one normalized lifecycle state:

| State | Meaning | Included in default current-context reads |
|---|---|---|
| `current` | Suitable for present-day context, subject to validation and expiry | Yes |
| `historical` | Accurate for a past period, but not current guidance | No |
| `superseded` | Replaced by one or more identified facts | No |
| `disputed` | Contested or unresolved and unsafe as default truth | No |

An explicit state is classification metadata, not a deletion or retention instruction.

Malformed explicit lifecycle metadata is represented as an invalid normalized view, not as one of these four valid states, and is excluded from current-context reads.

## Qdrant payload fields

Lifecycle metadata lives beside the existing fact payload fields:

| Field | Type | Contract |
|---|---|---|
| `lifecycle_state` | string | One of `current`, `historical`, `superseded`, or `disputed`. |
| `canonical` | boolean | An explicit preference hint. It is valid only on a `current` fact. |
| `provenance` | object | Origin metadata with required non-empty string `source` and optional string `reference`. |
| `verified_at` | string | Timestamp in RFC3339 format. |
| `supersedes` | array | Unique point IDs that this fact replaces. IDs are normalized as strings. |
| `superseded_by` | array | Unique point IDs that replace this fact. IDs are normalized as strings. Required when state is `superseded`. |

Relationship IDs must be non-empty string or non-negative integer point IDs. A fact cannot reference its own point ID. Duplicate IDs normalize to one occurrence. A `current` fact cannot have `superseded_by` entries.

Valid current fact metadata:

```json
{
  "lifecycle_state": "current",
  "canonical": true,
  "provenance": {
    "source": "user",
    "reference": "decision-7"
  },
  "verified_at": "2026-07-21T08:30:00Z",
  "supersedes": ["older-point-id"]
}
```

Valid superseded fact metadata:

```json
{
  "lifecycle_state": "superseded",
  "canonical": false,
  "superseded_by": ["current-point-id"]
}
```

### Legacy facts

A payload with none of the lifecycle fields is normalized as `current` with `legacy=true`:

```json
{
  "text": "Existing fact without lifecycle metadata"
}
```

This is the only legacy-current rule. Once any lifecycle field is present, the payload is no longer classified as legacy, even when `lifecycle_state` is omitted. An explicit unknown state or any malformed explicit lifecycle field has `valid=false` and a metadata-only `invalid_reason`. Invalid string states remain visible verbatim in inspection views; a non-string state is represented as an empty state rather than being mislabeled as `current`. Invalid metadata must never cause a panic or leak fact text into the reason.

The normalized view returned by read surfaces contains `state`, `legacy`, `canonical`, optional `provenance`, optional `verified_at`, `supersedes`, `superseded_by`, `valid`, and optional `invalid_reason`.

## Authority metadata

`canonical=true` is an explicit ranking hint, not a global uniqueness guarantee. It is accepted only for a valid `current` fact. Personal Memory does not currently enforce one canonical fact per topic, namespace, tag, or relationship set.

Provenance records origin, not trust. A source or reference does not make a fact correct, verified, canonical, or current. `verified_at` records when a fact was verified but does not independently change its state or authority.

## State changes and invariants

Lifecycle transitions are explicit, reversible, and idempotent. Reapplying the same valid target state is safe. Any known state may be corrected to another known state when the complete target metadata satisfies these invariants:

- the target state is one of the four defined states;
- `canonical=true` appears only with `current`;
- `superseded` has at least one `superseded_by` point ID;
- `current` has no `superseded_by` point IDs;
- provenance, verification time, and relationship IDs have valid types and formats;
- relationship arrays contain no empty, duplicate, or self-referencing normalized IDs.

Changing a lifecycle state does not automatically create, edit, or validate the related points. It also does not infer reciprocal relationships.

## Retention and expiry

`permanent=true` is retention-only: lifecycle-aware maintenance treats the fact as protected. It does not make the fact current, canonical, valid, verified, or visible in default context. Direct age-only deletion through `forget_old` is deprecated and refused.

`valid_until=YYYY-MM-DD` is an exact UTC calendar date, independent of lifecycle classification. A fact remains valid through that date and expires only on a later UTC date. Store and import reject malformed explicit expiry values; legacy malformed expiry payloads are never current context, although they remain inspectable through inventory surfaces. Once expired, even a valid canonical `current` fact is excluded from current-context flows. Expired facts remain available to inventory surfaces such as list, export, stats, and Viz.

## Read visibility

`recall_facts` accepts an optional `lifecycle_mode` with the closed values `current`, `history`, `as_of`, and `include_all`. Omitting the parameter is exactly equivalent to `current`, preserving existing client behavior. `as_of` is accepted only with `lifecycle_mode=as_of`, is required in that mode, and must be an exact, real `YYYY-MM-DD` calendar date. Supplying `as_of` in any other mode is an error.

| Read surface or mode | Current | Historical | Superseded | Disputed | Invalid lifecycle | Expired at reference date |
|---|---:|---:|---:|---:|---:|---:|
| `recall_facts`, `current` (default) | Include or demote | No | No | No | No | No |
| `recall_facts`, `history` | Include | Include | Include | Uncertain | No | No |
| `recall_facts`, `as_of` | Include | Include | Include | Uncertain | No | No |
| `recall_facts`, `include_all` | Include | Include | Include | Uncertain | No | No |
| Operational context tool and HTTP endpoint | Yes | No | No | No | No | No |
| `find_related` | Yes | Yes | Yes | Yes | Yes, labeled invalid | No |
| `list_facts` | Yes | Yes | Yes | Yes | Yes, labeled invalid | Yes |
| `export_facts` | Yes | Yes | Yes | Yes | Yes, raw payload | Yes |
| `get_stats` | Yes | Yes | Yes | Yes | Counted separately | Counted separately |
| Viz fact list, detail, graph, and duplicates | Yes | Yes | Yes | Yes | Yes, normalized invalid view | Yes |

All `recall_facts` modes exclude malformed lifecycle metadata and facts expired at the applicable reference date. Legacy facts qualify as valid current facts. The `current`, `history`, and `include_all` modes use the current UTC date as their expiry reference. The `as_of` mode uses its supplied date instead: a fact is valid through its `valid_until` date and is excluded only when `as_of` is later. `as_of` does not reconstruct historical intervals, infer a fact's state on that date, or use lifecycle transition history; it changes only the expiry reference.

All ordinary memory read surfaces also require valid maintenance status `active`; absence of maintenance metadata is legacy active. Explicitly quarantined or malformed maintenance records are not ordinary context. The read-only analyzer can still inspect every record so it can report quarantined and malformed metadata safely.

The broad modes (`history`, `as_of`, and `include_all`) expose all valid, non-expired lifecycle states. Current, historical, and superseded facts have the `include` decision with the safe reason codes `current_context`, `historical_context`, and `superseded_context` respectively. A disputed fact is always visibly uncertain: its decision is `uncertain` with reason code `disputed`. In default `current` mode, current facts are included with `current_truth`; when at least one canonical current candidate is present, ordinary and legacy-current candidates are demoted with `canonical_preference`.

The structured `recall_facts` response exposes each fact's unchanged `semantic_score`, original `semantic_rank`, lifecycle-adjusted `final_rank`, normalized `lifecycle` view, presentation `decision`, and closed, privacy-safe `reason_codes`. It also exposes `candidate_window_saturated`: when true, the bounded semantic candidate request filled its window, so a partial or empty presentation may be incomplete rather than a normal no-match result. The text fallback reports the same condition. `point_id` is available only in structured content; the backward-compatible text fallback omits point IDs and lifecycle relationship IDs. Lifecycle ranking operates on a bounded semantic candidate pool (at least 20 candidates, normally four times the requested limit, capped by the service search limit multiplier), then returns the requested limit. Canonical preference can therefore reorder only candidates already in that pool; it cannot retrieve an otherwise absent canonical fact, and it never changes vector similarity scores.

Cache entries are isolated by lifecycle mode and by the `as_of` date when applicable. Omitting `lifecycle_mode` shares the exact cache identity of explicit `current`; dates from one `as_of` request cannot satisfy another mode or date.

`find_related` is an inspection-oriented semantic surface: it keeps all lifecycle states, ranks lifecycle authority tiers, and exposes normalized lifecycle metadata in its output.

Viz privacy-safe summaries include the normalized lifecycle block. Graph inspection accepts closed lifecycle-state and explicit authority-signal filters alongside its existing namespace and tag filters. Selected detail responses use an explicit allowlist of summary fields, operational timestamps, safe text-origin metadata, and redacted lifecycle provenance; they never return the raw Qdrant payload or unrelated payload fields. The separate history response resolves a bounded, deterministic supersession chain and represents missing, unavailable, cyclic, and truncated relationships explicitly. All ordinary Viz views are maintenance-active-only. Authenticated operators can explicitly inspect valid quarantined facts through the read-only `GET /viz/api/facts?maintenance_status=quarantined` selector on Overview; it returns the existing ordinary fact summary fields (including fact text and point ID) plus normalized `maintenance_status`, `quarantined_at`, `quarantine_reason`, and `quarantine_batch_id`. This protected operator UI/API is not content-free; CLI maintenance manifests, journals, and command output remain content-free and never include fact text or vectors. The inspection has no mutation controls and never includes malformed maintenance metadata as quarantined.

## Lifecycle-aware maintenance analysis

`cmd/maintenance analyze` performs one read-only collection scan and writes an exclusive mode-`0600` manifest. Candidate classes are expired, superseded past a configured retention interval, deterministic duplicate candidate, stale/unused review candidate, malformed/orphaned metadata, protected, and already quarantined. Reports contain IDs, closed classes, safe timestamps, policy settings, and payload fingerprints, never fact text or vectors.

Permanent, disputed, and current canonical records are protected. Age or low recall activity alone is review-only and never makes a record eligible for quarantine. Superseded retention is measured from the server-recorded `lifecycle_transitioned_at`, never the unrelated content `updated_at`. A superseded record without a valid transition timestamp is insufficient evidence for retention eligibility and is reported as malformed/review-only.

`forget_old(dry_run=true)` remains only as a content-free compatibility preview. `dry_run=false` fails before scanning or mutation.

## Manifest-bound quarantine and restore

`cmd/maintenance quarantine` and `restore` are explicit operator actions. Stop `memory-mcp` and every other collection writer first and keep them stopped until the action completes; each command requires `--confirm-server-stopped` because a standalone CLI cannot invalidate another process's recall cache. Each also requires `--qdrant-url`, `--collection`, a saved `--manifest`, and a private `--journal`; quarantine additionally accepts repeatable `--point-id` or `--eligible`. Restore accepts only explicit IDs. The command rejects unknown or trailing JSON in a bounded manifest, mismatched schema/policy/batch/collection values, IDs absent from the manifest, and duplicate selections. Protected or ineligible records produce a per-point `protected_or_ineligible` outcome, while payload-fingerprint drift produces a per-point `conflict` outcome.

The service writes only the complete typed maintenance shape: quarantine sets `maintenance_status=quarantined` with a closed reason, timestamp, and manifest batch ID; restore sets `maintenance_status=active` and removes all quarantine-only fields. It re-reads after every dispatched write. Qdrant has no arbitrary payload-fingerprint compare-and-set, so a concurrent writer can leave an irreducible race; any failed post-write verification is recorded as `ambiguous`, not `updated`. Result journals contain only operation, batch ID, point IDs, and closed statuses, use mode `0600`, and are atomically replaced. After a dispatched mutation, journal persistence uses a separate bounded cleanup context so request cancellation does not erase an ambiguous audit outcome.

## Manual phase-three purge

Purge is manual-only and permanently deletes only explicit point IDs from a complete saved manifest. It is unavailable through MCP, Viz, scheduled jobs, and automation wrappers. Any production purge belongs to a separate, explicitly approved maintenance window after deployment; this implementation and its tests perform no production purge.

The operator must stop every writer, use `--confirm-server-stopped`, retain the original manifest and private mode-`0600` journal, provide a private `--snapshot-archive` path outside Qdrant's managed snapshot directory, give `--confirm-purge`, give a positive bounded `--minimum-quarantine-days`, and repeat `--point-id` for every target. `--eligible` is never accepted for purge. Protected records and review-only age/low-recall candidates remain ineligible. Each selected record is re-read and must still be valid quarantined metadata with the matching manifest batch and reason, the completed minimum quarantine age, and the original payload fingerprint.

Before any deletion, purge creates a fresh Qdrant snapshot, proves its exact identity exists, downloads it to the required operator-controlled archive, fsyncs the mode-`0600` file, and records its SHA-256 in the journal. The archive must live outside Qdrant's managed snapshot directory and is therefore not subject to scheduled `KEEP_SNAPSHOTS` pruning; the CLI rejects the standard Compose-managed `/qdrant/snapshots` tree. Purge re-proves the live snapshot identity immediately before each deletion. Any `failed`, `ambiguous`, `conflict`, `not_found`, or `protected_or_ineligible` outcome is unresolved and makes the command nonzero; do not restart writers until the journal is inspected. Retry the original explicit selection with the same manifest, journal, and archive path. A partial retry reuses the original pre-delete snapshot identity and verifies the same archive checksum rather than taking a snapshot after partial deletion. If recovery is required, restore the recorded archive with the approved Qdrant collection recovery procedure while writers are stopped, verify the collection, and only then restart and check health/logs.

## Rollout, future writes, and rollback

Deployment remains read-only and performs no startup migration. Existing payloads are neither rewritten nor backfilled automatically. Missing lifecycle metadata remains compatible through legacy normalization, so deployment does not require a collection-wide mutation.

`store_fact` and `update_fact` accept optional lifecycle inputs. Omitting every lifecycle input preserves the legacy-compatible store behavior and the existing update metadata respectively. Supplying any lifecycle input constructs a complete explicit target: the state defaults to `current`, canonical defaults to false, relationships default to empty, and omitted provenance or verification metadata is absent.

`set_fact_lifecycle` is the metadata-only transition path. It requires an exact numeric or UUID point ID, never performs semantic target selection, and never calls the embedding service. The request describes a complete target lifecycle view. Qdrant updates are restricted to lifecycle keys, so text, vectors, recall counters, and unrelated payload fields are not rewritten. Changing a state does not infer or maintain reciprocal relationships.

`store_fact` never overwrites an existing deterministic point ID: an ordinary active record returns `collision`, while an inactive/quarantined record returns `inactive_collision` and remains reserved for the maintenance restore workflow. A lookup dependency failure returns `dependency_failed`, never a collision. `import_facts` preserves valid lifecycle metadata from exported facts and returns a structured, per-item outcome (`stored`, `duplicate`, `collision`, `inactive_collision`, `dependency_failed`, `invalid`, `embedding_failed`, `inconclusive`, or `write_ambiguous`) with an equivalent text fallback. Entries with malformed explicit lifecycle metadata or expiry are `invalid` without logging their fact text. A `write_ambiguous` outcome is not success and must be verified before retrying. Duplicate candidates include their valid `valid_until` date so callers can decide whether renewal is appropriate.

### Explicit legacy migration

`personal-memory-migrate-lifecycle` classifies only payloads with none of the six lifecycle keys. Its sole deterministic target is explicit `current`, `canonical=false`, and empty relationship arrays. Expiry, retention, text, tags, and similarity do not influence classification.

The command is dry-run-only unless `-apply` is supplied. Apply requires every memory writer to be stopped and an exclusive rollback manifest path. The manifest is created with mode `0600` before the first mutation and contains point IDs plus lifecycle-only before/after metadata; it never contains fact text or vectors. A partial apply resumes from the same immutable manifest.

Before production apply:

1. Create and verify a Qdrant snapshot of the memory collection.
2. Stop every server, importer, migration, and other memory writer.
3. Run the lifecycle migration without `-apply` and review its counts and point IDs.
4. Run apply with `-confirm-writes-stopped` and a new `-rollback-manifest` path.
5. Re-run apply with the same manifest to verify zero remaining changes before restarting writers.

Rollback also requires stopped writers. It restores a point only when its current lifecycle subset still exactly matches the migration-applied target. A deliberate post-migration change is reported as a conflict and is never overwritten. If manifest rollback cannot be completed, restore the pre-migration Qdrant snapshot according to the infrastructure runbook.
