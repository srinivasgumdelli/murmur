# Murmur Memory — Design

Status: draft, 2026-11-26

## Summary

Add shared, persistent memory to murmur so agents can keep project knowledge across sessions and hand it to each other: test commands, conventions, decisions, pitfalls. Memory is a second primitive next to messages, with its own tables and endpoints. It is built into murmur rather than adopted from an external project, it works in plain murmur with no other system, and agents are pushed to use it through Claude Code hooks rather than instructions alone.

## Background

`DESIGN.md` rejects "shared memory / knowledge graph" as the wrong abstraction for agent communication: communication is chat, not search. That still holds. Memory does not replace messages and is not a kind of message. It answers a different need: knowledge that should outlive a conversation and be found later by agents that were not part of it.

Today that knowledge lands in per-agent files (for example Claude Code's local memory directory), in repo notes, or nowhere. It is invisible to other agents, lost when a sandbox is thrown away, and has no record of who wrote it.

## Goals

- Agents can write, search and update shared memory over HTTP and MCP.
- Every entry records who wrote it, verified from the API key, not from the request body.
- Entries are scoped (workspace, task, shared) so one task's notes don't flood another's.
- Agent-written entries are treated as claims. Only a lead or a person can promote an entry into the shared pool.
- History is kept: edits add rows, deletes expire rows.
- Agents receive relevant memory at session start without having to ask, and are made to record what they learned before they stop.
- Works in standalone murmur. A host that embeds murmur can add stronger guarantees on top.

## Non-goals

- Automatic fact extraction or consolidation by an LLM on the write path.
- Knowledge graphs, entity relations, or temporal fact tracking.
- Vector search in the first version (planned, see Search).
- Judging the quality of what agents write.

## Decision: build in murmur, don't adopt

Mature open-source memory projects exist (Mem0, Graphiti/Zep, Cognee, Letta, Basic Memory). They were considered and not adopted, for these reasons:

- **The hard part here is trust, not recall.** Murmur needs provenance bound to API keys, a split between claims and promoted facts, scopes, append-only history and audit hooks. None of the projects model these, so they would have to be wrapped, and the wrapper is most of the work.
- **Deployment weight.** Most are Python services, several need an extra database (Neo4j, Qdrant, Postgres) and most call an LLM on every write. Murmur is a single Go binary with one database.
- **LLM on the write path is a liability.** It needs model credentials, costs money per write, and rewrites agent text before storage, which blurs who claimed what.
- **The need is modest.** Coding agents mostly need short keyed notes. Full-text search covers that.
- **Small build.** One table, a handful of endpoints, text search and notifier hooks, about the size of the existing messages handler.

**Revisit when** any of these shows up in real use:

- Text search misses relevant entries often enough that agents repeat work (first add vectors; adopt an engine only if that is not enough).
- Memory grows past what people can curate, and duplicate or stale entries need automatic merging.
- Agents need relationship queries across entities.

The store interface (see Storage) keeps this open: an adapter for an external engine can sit behind it without changing the trust model.

## Threat model

Shared memory is a channel from one agent into every later agent's context. The main risks:

1. **Impersonation.** An agent writes entries under another agent's name, for example the lead's, to make them look trusted.
2. **Poisoning.** A compromised or confused agent writes false or instruction-like entries ("always skip tests", "push to main") that later agents act on.
3. **Silent rewrite.** An agent edits or deletes entries to hide what was there.
4. **Open writes.** With auth off, anyone who can reach murmur can write memory.

Responses:

1. Author comes from the API key only (see Prerequisite).
2. Agent-written entries are stored with `provenance = 'reported'` and presented to agents as claims, labelled with their author. Only promoted entries are presented as project facts. Promotion requires the lead role or the admin key.
3. Entries are never updated in place. An edit inserts a new row with `supersedes` pointing at the old one. Delete sets `expires_at`. Admin-only purge exists for real removal and is logged.
4. Memory endpoints refuse to run unless auth is `required`. Murmur's current default is `MURMUR_AUTH=off`, and `optional` lets invalid keys through (`internal/handler/auth.go:122`). With either setting, `/memories` returns `503` with an explanation.

## Prerequisite: bind sender to API key

`AuthMiddleware` looks up the agent that owns a key and discards it. `POST /messages` trusts the `sender` field in the body. This is a gap for chat and a hole for memory.

Fix, shipped as its own change before memory:

- `AuthMiddleware` stores the key's agent in the request context.
- `POST /messages` rejects a request whose `sender` differs from the key's agent (`403`). The admin key may still send as any agent.
- Memory handlers ignore any author in the body and use the context value.

## Data model

The logical model is the same for every backend. The SQL below is the Postgres backend's; the SQLite backend is sketched after it. Handlers never write SQL directly.

```sql
-- Postgres backend
CREATE TABLE memories (
    id           SERIAL PRIMARY KEY,
    scope        TEXT NOT NULL,          -- 'workspace:<id>' | 'task:<id>' | 'shared'
    key          TEXT,                   -- optional stable key, e.g. 'cmd.test'
    kind         TEXT NOT NULL DEFAULT 'note',
                                         -- note | command | decision | pitfall | none
    body         TEXT NOT NULL,
    tags         TEXT[] DEFAULT '{}',
    author       TEXT NOT NULL,          -- from API key
    session_id   TEXT,                   -- optional, for tracing
    provenance   TEXT NOT NULL DEFAULT 'reported',  -- reported | promoted
    promoted_by  TEXT,
    supersedes   INT REFERENCES memories(id),
    created_at   TIMESTAMPTZ DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    body_tsv     TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', body)) STORED
);

CREATE INDEX idx_memories_scope_key ON memories (scope, key);
CREATE INDEX idx_memories_author_created ON memories (author, created_at);
CREATE INDEX idx_memories_tsv ON memories USING GIN (body_tsv);
```

```sql
-- SQLite backend (sketch)
CREATE TABLE memories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL, key TEXT, kind TEXT NOT NULL DEFAULT 'note',
    body TEXT NOT NULL, tags TEXT NOT NULL DEFAULT '[]',   -- JSON array
    author TEXT NOT NULL, session_id TEXT,
    provenance TEXT NOT NULL DEFAULT 'reported', promoted_by TEXT,
    supersedes INTEGER REFERENCES memories(id),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    expires_at TEXT
);
CREATE INDEX idx_memories_scope_key ON memories (scope, key);
CREATE INDEX idx_memories_author_created ON memories (author, created_at);
CREATE VIRTUAL TABLE memories_fts USING fts5(body, content='memories', content_rowid='id');
-- plus insert/delete triggers keeping memories_fts in sync
```

The SQLite backend runs in WAL mode with short write transactions. One writer at a time is fine for the expected handful of agents.

Rules:

- **Current entry:** the latest row for a `(scope, key)` pair that is not expired and not superseded. Entries without a key are each their own entry.
- **Upsert by key:** writing with an existing `(scope, key)` inserts a new row that supersedes the current one.
- **`kind = 'none'`** records that an agent had nothing worth saving. It counts as a write for the stop gate and is excluded from search and injection.
- **Size limit:** `body` capped at 4 KB. Memory holds short notes, not documents.

## API

All endpoints require auth `required` and a valid key.

### `POST /memories`

```json
{
  "scope": "workspace:api-server",
  "key": "cmd.test",
  "kind": "command",
  "body": "Run tests with `make test`; `go test ./...` misses the integration tag.",
  "tags": ["testing"],
  "session_id": "optional",
  "ttl_days": 90
}
```

Returns the stored entry. Author and provenance are set by the server.

### `GET /memories`

Query parameters: `scope` (repeatable or comma-separated), `q` (full-text), `tags`, `kind`, `author` (`me` resolves to the key's agent), `since`, `include_history`, `limit` (default 20, max 100).

Returns `{ "items": [...], "total": n }`. By default only current, non-expired entries, excluding `kind = 'none'`. `total` lets hooks count without fetching rows.

### `GET /memories/{id}`

One entry, with its `supersedes` chain when `include_history=true`.

### `POST /memories/{id}/promote`

Lead role or admin key only. Copies the entry into `scope = 'shared'` with `provenance = 'promoted'` and `promoted_by` set, superseding any current shared entry with the same key.

### `DELETE /memories/{id}`

Sets `expires_at = now()`. The author, a lead, or the admin key may expire an entry. Promoted entries can only be expired by a lead or the admin key.

### `POST /memories/{id}/purge`

Admin key only. Removes the row. Logged.

## Search

- **Postgres (now):** `tsvector` with a GIN index, ranked by `ts_rank`, then recency. Promoted entries rank above reported ones at equal relevance.
- **SQLite (lands with murmur's SQLite store):** FTS5 with its built-in BM25 ranking, then the same recency and provenance rules. Ranking scores differ slightly from Postgres; tests check order of results for clear cases, not exact scores.
- **Vectors (later):** `pgvector` and `sqlite-vec`, behind the same interface. Embeddings need a model call, so murmur must be able to call a configured embedding endpoint without agents holding the credential. Deferred until text search proves insufficient.

## Storage

A `MemoryStore` interface sits below the trust layer:

```go
type MemoryStore interface {
    Put(ctx context.Context, m Memory) (Memory, error)
    Get(ctx context.Context, id int, withHistory bool) ([]Memory, error)
    Search(ctx context.Context, q Query) (items []Memory, total int, err error)
    Promote(ctx context.Context, id int, by string) (Memory, error)
    Expire(ctx context.Context, id int, at time.Time) error
    Purge(ctx context.Context, id int) error
}
```

Author binding, permission checks, the auth-mode guard, size limits and notifications live in a service layer above the interface, shared by the HTTP handlers and MCP tools, so they apply to any backend: Postgres now, SQLite later, an external engine if ever adopted.

One behaviour test suite runs against every backend (author binding, upsert by key, history, promotion, expiry, search order, counts). A backend isn't done until it passes the whole suite.

## Notifications

Writes, promotions and expiries publish on the existing notifier as `memory` events (`{id, scope, key, kind, provenance}`). Agents already streaming messages can subscribe to memory events by scope. A host embedding murmur hooks the same events into its audit trail.

## Agent access

- **MCP tools**, added to murmur's MCP surface: `memory_search(q, scope?, tags?)`, `memory_put(body, key?, kind?, scope?, tags?)`, `memory_expire(id)`. Agents use first-class tools far more reliably than curl instructions.
- **murmur-connect skill:** documents the tools and states the rule: "Project knowledge lives in murmur memory. Don't write it to local files."
- Injected memory is framed for the agent: promoted entries under "Project facts", reported entries under "Notes from other agents (unverified)", each with its author.

## Enforcement with hooks

A model can't be made to call a tool. Hooks make reads automatic, force a write before stopping, capture mechanical facts without the model, and block the alternatives. The hooks ship with the murmur-connect skill.

| Purpose | Hook | Strength |
|---|---|---|
| Load memory | `SessionStart` (startup, resume, compact) | Full: the model doesn't decide |
| Record mechanical facts | `PostToolUse` on Bash | Full: the hook writes, not the model |
| Record judgment | `Stop` gate | Forces a write; can't judge its quality |
| Block local memory files | `PreToolUse` on Write/Edit | Full, if hooks can't be edited |

### Load: `SessionStart`

Fetches current entries for the agent's workspace, task and shared scopes, and returns them as `additionalContext`. Runs on startup, resume and after compaction, so memory survives long sessions. It also records the session start time for the stop gate.

```bash
#!/usr/bin/env bash
# hooks/memory-load.sh
input=$(cat)
sid=$(jq -r .session_id <<<"$input")
date -u +%Y-%m-%dT%H:%M:%SZ > "${TMPDIR:-/tmp}/murmur-mem-$sid"

curl -sf -H "Authorization: Bearer $MURMUR_KEY" \
  "$MURMUR_URL/memories?scope=workspace:$MURMUR_WORKSPACE,shared&limit=50" |
jq '{hookSpecificOutput: {hookEventName: "SessionStart",
     additionalContext: ("Project memory (murmur):\n" +
       (.items | map("- [" + .provenance + ", " + .author + "] " + .body) | join("\n")))}}'
```

### Write gate: `Stop`

Counts entries this agent wrote since the session started. If there are none, it blocks stopping and tells the agent to write something, or an explicit `none`. `stop_hook_active` limits this to one retry, so a stubborn agent can't loop forever.

The gate counts by author and start time, not by Claude Code's session id. The agent's MCP calls don't know that id, so a session-id filter would never match.

```bash
#!/usr/bin/env bash
# hooks/memory-gate.sh
input=$(cat)
[ "$(jq -r .stop_hook_active <<<"$input")" = "true" ] && exit 0
sid=$(jq -r .session_id <<<"$input")
since=$(cat "${TMPDIR:-/tmp}/murmur-mem-$sid" 2>/dev/null) || exit 0

total=$(curl -sf -H "Authorization: Bearer $MURMUR_KEY" \
  "$MURMUR_URL/memories?author=me&since=$since&kind=any&limit=1" | jq .total)
[ "${total:-0}" -gt 0 ] && exit 0

jq -n '{decision: "block",
  reason: "Before stopping: save anything future agents need with memory_put (commands, decisions, pitfalls). If nothing is worth saving, call memory_put with kind \"none\"."}'
```

`kind=any` includes `none` entries in the count.

### Mechanical capture: `PostToolUse`

Writes facts a script can recognise, with no model involved:

- A successful test or build command (`make test`, `go test`, `npm run build`, `pytest`) is stored as `kind = 'command'` under a stable key such as `cmd.test`, so repeats update one entry.
- Later: a command that failed and then passed after a change, stored as `kind = 'pitfall'`. Harder to detect reliably, so it's not in the first version.

### Block alternatives: `PreToolUse`

Denies `Write` and `Edit` to local memory locations: `*/.claude/projects/*/memory/*`, `MEMORY.md`, and configured repo notes paths. Returns `permissionDecision: "deny"` with a reason pointing to `memory_put`.

Also turn off Claude Code's built-in auto-memory in agent settings. Confirm the exact setting name against current Claude Code docs before documenting it.

### Optional: transcript harvest

A `SessionEnd` hook may send the transcript path to a summarizer run by the lead or a host process, which writes entries as `reported` under the summarizer's own identity. This is the only place an LLM extracts facts, and it stays off the agent write path. Not in the first version.

## Standalone vs embedded

Memory is a murmur feature and runs wherever murmur runs.

**Plain murmur provides:** the full API, MCP tools and hooks, author binding, history, promotion gated by the lead role or admin key, and live memory events.

**Limits in plain murmur:**

- Keys are created by hand through `/keys`.
- Hooks are installed by the user in agent settings, and an agent with shell access could edit them. Enforcement holds for cooperative agents, not hostile ones.
- An agent with database access could bypass the API. Run the database where agents can't reach it.

**A host that embeds murmur** (for example, one that runs agents in sandboxes) can add:

- Per-agent keys issued automatically at agent start.
- Hooks, MCP config and settings shipped in the agent image or mounted read-only, so agents can't edit them.
- Ephemeral agent filesystems, so anything written outside murmur disappears.
- Memory events written into a tamper-evident audit trail.
- A console for browsing memory and promoting entries, with stronger approval for promotion.

## Retention

- `ttl_days` per write, default 90 for `reported` entries. Promoted entries don't expire unless set.
- The existing reaper also expires old memory rows. Expired rows are kept for 30 days for history, then purged.
- Per-scope cap (default 1,000 current entries). Writes past the cap return `429` with a hint to expire or consolidate.

## Observability

- `/health` and `GET /memories/stats` report counts per scope, writes per agent, the share of `none` writes, and searches per session.
- A session that ends with only `none` writes over many tasks suggests the gate is being satisfied without real content. Surface it for review.

## Rollout

1. **Sender binding fix.** Separate PR, useful on its own.
2. **Memory core.** `MemoryStore` interface and the backend test suite, Postgres backend, endpoints, auth-mode guard, text search, notifier events. Tests for author binding, upsert-by-key, history, promotion permissions and expiry.
3. **SQLite backend.** Passes the same suite. Becomes the default when murmur's message store moves to SQLite (store interface, pure-Go SQLite, in-process notifier), so both stores change backend in one step.
4. **Hooks and skill.** `SessionStart` load, `Stop` gate, `PreToolUse` deny, `PostToolUse` command capture, with install instructions in the murmur-connect skill. Agents call the HTTP API until MCP tools exist.
5. **MCP tools.** Once murmur has an MCP endpoint.
6. **Later.** Vectors (`pgvector`, `sqlite-vec`), transcript harvest, pitfall detection.

## Implementation plan

Each phase ends with passing tests and its own PR. Tests are written before the code they cover. Sizes are for one developer and assume nothing surprising in the existing code. Total for phases 0–4 and 6 is about 8–9 days. Phase 5 adds 1 day if murmur already has an MCP endpoint, or 3–4 days if it has to be built.

### Package layout

```
internal/memory/
  memory.go          Memory, Query, Kind, Provenance, errors
  store.go           MemoryStore interface
  service.go         trust layer: author binding, permissions, caps, auth guard, events
  storetest/suite.go shared behaviour suite: Run(t, newStore func(t) MemoryStore)
  pgstore/           Postgres backend (pgx)
  sqlitestore/       SQLite backend (pure Go, FTS5)
internal/handler/memories.go   HTTP handlers, thin: decode, call service, encode
skills/murmur-connect/hooks/   hook scripts and a settings snippet
```

The trust rules sit in `memory.Service`, not in the HTTP handlers, so the HTTP API and the MCP tools apply the same rules. Handlers and MCP tools only translate requests.

### Phase 0 — Spikes (½ day)

Answer the unknowns before writing code that depends on them.

- **0.1** Confirm the pure-Go SQLite driver (`modernc.org/sqlite`) supports FTS5 with external-content tables and the `bm25()` function. If it doesn't, decide between a cgo driver (`mattn/go-sqlite3` with the `sqlite_fts5` tag) and a fallback to `LIKE` search. The pure-Go requirement matters for cross-compiling.
- **0.2** Check current Claude Code hook input and output formats for `SessionStart` (including the `compact` source), `Stop` (`stop_hook_active`, `decision: "block"`), `PreToolUse` (`permissionDecision: "deny"`) and `PostToolUse` (tool input and exit status available to the hook). Record the findings in the skill's hook README.
- **0.3** Find the exact Claude Code setting that disables auto-memory.
- **0.4** Decide the open question on the lead role. Default unless decided otherwise: an agent is a lead when `agents.role = 'lead'`.

Done when: notes for 0.1–0.4 are written into this spec's open questions, either answered or with a decision made.

### Phase 1 — Bind sender to API key (½ day, own PR)

- **1.1** `AuthMiddleware` puts the key's agent into the request context (`handler.AgentFromContext`), and marks admin-key requests as admin.
  Tests: valid key sets the agent; admin key sets admin; invalid key with `optional` sets nothing.
- **1.2** `POST /messages` returns `403` when `sender` differs from the context agent. Admin may send as anyone. With auth `off`, behaviour is unchanged.
  Tests: matching sender succeeds, mismatched sender gets `403`, admin as another agent succeeds, auth off unchanged.
- **1.3** Apply the same check to agent registration and deletion, so an agent can't register or remove another agent's record.
- **1.4** README: note that `optional` auth doesn't stop impersonation, and recommend `required`.

Done when: all handler tests pass, including the existing poll tests.

### Phase 2 — Memory core on Postgres (3 days)

- **2.1 Types and interface.** `memory.go` and `store.go` as in Storage. Validation helpers: scope format (`workspace:<id>`, `task:<id>`, `shared`), kind values, 4 KB body limit, key format.
  Tests: validation table tests.
- **2.2 Shared suite.** `storetest.Run` covers:
  - put and get, with server-set fields kept;
  - upsert by `(scope, key)` supersedes and `Get` with history returns the chain;
  - search returns only current, non-expired entries, excluding `none` unless asked;
  - full-text ranking order for clear cases, with promoted entries above reported ones at equal relevance;
  - filters by scope, tags, kind, author and `since`;
  - `total` is correct with `limit`;
  - promote copies into `shared` with provenance `promoted` and supersedes the earlier shared entry with the same key;
  - expire hides an entry and purge removes it;
  - concurrent writes to the same key leave exactly one current entry.
- **2.3 Postgres backend.** Schema added in `pgstore` (applied from `schema.Apply`), queries, `ts_rank` plus recency ordering, a transaction for upsert with `SELECT … FOR UPDATE` on the current row.
  Tests: `storetest.Run` against a real Postgres from `MURMUR_TEST_PG_URL`, skipped locally when unset, always run in CI.
- **2.4 Service layer.** `memory.Service` wraps a store:
  - refuses everything unless auth mode is `required` (returns `ErrAuthRequired`, mapped to `503`);
  - sets author and provenance from context, ignoring the body;
  - promote only for lead or admin;
  - expire only by the author, a lead or admin, and promoted entries only by a lead or admin;
  - purge only by admin;
  - per-scope cap returns `ErrScopeFull` (`429`);
  - publishes an event after each successful write.
  Tests: a fake store; one table test per rule.
- **2.5 HTTP handlers.** `POST /memories`, `GET /memories`, `GET /memories/{id}`, `POST /memories/{id}/promote`, `DELETE /memories/{id}`, `POST /memories/{id}/purge`, `GET /memories/stats`. Routes added in `cmd/murmur/main.go`. `author=me` resolves from context, and `kind=any` includes `none`.
  Tests: `httptest` against the service with a fake store, covering status codes, JSON shape and query parsing.
- **2.6 Events.** Add a typed publish to `Notifier` for memory events, called in-process by the service. No database trigger, so it works the same on both backends. Expose memory events on `/messages/stream` behind a `types=memory` parameter, so existing clients see no change.
  Tests: a write wakes a subscribed waiter; existing stream tests unchanged.
  Limitation: events reach only clients of the same murmur process. That is fine for a single instance, which is murmur's deployment model.
- **2.7 Retention.** Extend `StartReaper`: expire entries past `expires_at` (default 90 days for `reported`), purge expired rows after 30 days. Settings: `MURMUR_MEMORY_TTL_DAYS`, `MURMUR_MEMORY_KEEP_EXPIRED_DAYS`, `MURMUR_MEMORY_SCOPE_CAP`.
  Tests: suite cases for expiry by time using an injected clock.
- **2.8 Stats and health.** `/memories/stats` returns counts per scope, writes per agent over 7 days, the share of `none` writes, and searches per agent. `/health` adds a memory store check.
- **2.9 CI.** Add a Postgres service to CI and set `MURMUR_TEST_PG_URL`.

Done when: the suite passes on Postgres in CI, and a manual curl run of every endpoint works with auth `required` and returns `503` with auth `off`.

### Phase 3 — SQLite backend (1½–2 days)

- **3.1 Schema.** As in Data model: the `memories` table, an FTS5 external-content table, and insert/update/delete triggers keeping it in sync. WAL mode, `busy_timeout` set, foreign keys on. Tags stored as a JSON array and filtered with `json_each`.
- **3.2 Queries.** FTS5 `MATCH` with `bm25()`, then recency and provenance ordering to match Postgres. Upsert in a `BEGIN IMMEDIATE` transaction, which takes the write lock up front and plays the role of `FOR UPDATE`. Escape user queries so FTS5 syntax characters can't cause errors.
  Tests: `storetest.Run` against a temp-file database, always run, no external service. Add an FTS-syntax test (quotes, `*`, `-`, `NEAR`) so bad input returns results or an empty list, never a `500`.
- **3.3 Store selection.** Setting `MURMUR_MEMORY_STORE=postgres|sqlite`. The default follows the message store: Postgres today, SQLite once the message store moves. `MURMUR_MEMORY_DB` sets the SQLite file path (default `./murmur-memory.db`). Running memory on SQLite while messages stay on Postgres is allowed for testing, but not documented as a deployment.
- **3.4 Coordination with the message store move.** When murmur's message store moves to SQLite (store interface, pure-Go SQLite, in-process notifier replacing `LISTEN/NOTIFY`), the two stores share one SQLite file and connection pool, and the default switches for both at once. If memory lands first, the memory backend is already in place and the move only changes the default. If the message move lands first, phase 3 reuses its connection setup.
- **3.5 CI.** Run the suite against both backends as a matrix. A backend change can't merge unless both pass.

Done when: the suite passes on SQLite and Postgres in CI, and murmur starts with `MURMUR_MEMORY_STORE=sqlite` and serves every endpoint.

### Phase 4 — Hooks and skill (1½ days)

- **4.1 Scripts** in `skills/murmur-connect/hooks/`: `memory-load.sh` (SessionStart), `memory-gate.sh` (Stop), `memory-capture.sh` (PostToolUse on Bash), `memory-guard.sh` (PreToolUse on Write and Edit). They read `MURMUR_URL`, `MURMUR_KEY` and `MURMUR_WORKSPACE`, and need only `bash`, `curl` and `jq`.
- **4.2 Failure behaviour.** If murmur is unreachable, load and capture exit 0 silently, and the gate exits 0 with a warning on stderr. A murmur outage must never trap an agent in a session.
- **4.3 Capture rules.** A table of command patterns and keys (`make test`, `go test`, `npm test`, `pytest` → `cmd.test`; `make build`, `go build`, `npm run build` → `cmd.build`; `golangci-lint`, `eslint`, `ruff` → `cmd.lint`). Only successful runs are recorded. Arguments are stored, environment variables are not, to avoid leaking secrets.
- **4.4 Settings snippet.** `hooks.json` to merge into `.claude/settings.json`, with matchers: SessionStart (`startup|resume|compact`), Stop, PostToolUse (`Bash`), PreToolUse (`Write|Edit`). Also the auto-memory setting from 0.3.
- **4.5 Tests.** A Go test starts a fake murmur (`httptest`) and runs each script with recorded hook inputs, checking its output JSON, the requests sent, and exit codes. Cases: memory present or empty; gate with zero writes, one write and `stop_hook_active`; capture on success and on failure; guard on memory paths and ordinary paths; murmur down.
- **4.6 Skill text.** Update `SKILL.md`: what memory is for, the HTTP calls until MCP exists, the rule against local memory files, how to install the hooks, and the required auth setting.
- **4.7 Manual check.** One real Claude Code session against a local murmur: memory appears at start, the gate blocks once, a test run is captured, a write to `MEMORY.md` is denied.

Done when: script tests pass in CI and the manual check is recorded in the PR.

### Phase 5 — MCP tools (1 day, plus the endpoint if missing)

- **5.1** If murmur has no MCP endpoint yet, add a streamable-HTTP MCP endpoint using the same API-key auth (2–3 days, shared with the message tools planned for it).
- **5.2** Tools: `memory_search(q, scope?, tags?, kind?, limit?)`, `memory_put(body, key?, kind?, scope?, tags?)`, `memory_expire(id)`, all calling `memory.Service`.
  Tests: tool calls through an MCP test client; each service rule checked once through MCP to confirm it is enforced the same way.
- **5.3** Skill: switch from HTTP instructions to the tools, and keep the HTTP section as a fallback.

### Phase 6 — Docs (½ day)

- `DESIGN.md`: messages are chat, memory is search, two primitives. Add memory to the architecture and endpoint sections.
- `README.md`: memory quick start, required auth, store selection settings, hook install.
- This spec: mark answered open questions, and record any deviations found during implementation.

### Risks

- **Pure-Go FTS5 support** (phase 0.1). If it's missing, the SQLite backend needs cgo or weaker search. Checked first so it can't block late.
- **Hook formats change.** Claude Code hook I/O may change between versions. The script tests pin current behaviour, and the skill states the Claude Code version it was checked against.
- **Gate fatigue.** Agents may learn to answer `none` every time. Watch the `none` share in stats, and revisit the wording before adding stronger rules.
- **Search quality.** If agents keep missing entries, add vectors (phase "Later") before considering an external engine.

## Open questions

- How agents learn their workspace id: from an env var set at launch (`MURMUR_WORKSPACE`), or derived from the git remote.
- Whether the `lead` role used for promotion is a new value in `agents.role` or comes from groups.
- Whether search should also return relevant recent messages, or stay strictly separate.
- Default caps (4 KB body, 1,000 entries per scope, 90-day TTL) need checking against real use.

## Phase 0 decisions

- Launchers set `MURMUR_WORKSPACE` explicitly and may set `MURMUR_TASK`. This keeps scope names stable across checkouts.
- The admin grants promotion authority by registering an agent with `role=lead` using the admin key. Registration by an ordinary agent can set its display role but cannot grant promotion authority. Existing lead records require an admin registration to acquire the grant.
- Memory search returns memory entries. Existing message endpoints continue to serve chat history.
- The initial defaults are a 4 KiB body, 1,000 current entries per scope, and a 90-day TTL for reports. Review them with deployment data.
- Claude Code documents startup, resume and compact session-start sources and `additionalContext`, the one-retry stop decision, and the write/edit denial response. Bash's successful post-tool hook has a structured result with interruption status. Disable built-in auto-memory with `autoMemoryEnabled: false`. [Claude Code hooks](https://code.claude.com/docs/en/hooks) and [Claude Code memory](https://code.claude.com/docs/en/memory).
- The FTS5 spike passed with `CGO_ENABLED=0` and `modernc.org/sqlite` v1.63.0 using WAL, an external-content table, an insert trigger, and a ranked query with `bm25()`. [SQLite driver](https://pkg.go.dev/modernc.org/sqlite@v1.63.0).
