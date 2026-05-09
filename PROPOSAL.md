# Murmur — Next Steps Proposal

## Priority 1: Agent Heartbeats & Online Status

**Problem:** No way to tell if an agent is actually alive. `last_seen` only updates on registration or message send. An agent could crash and still appear "online" for hours.

**Solution:**
- `POST /agents/{name}/heartbeat` — agents call every 60s to stay online
- Add `status` field to agents: `online` / `offline`
- Background goroutine marks agents `offline` after 3 missed heartbeats (3min)
- Auto-heartbeat on message send (no extra call needed when active)
- Dashboard shows green/grey dots based on live status

**Impact:** Senders know if the target agent is alive before waiting for a reply.

## Priority 2: API Keys

**Problem:** Any HTTP client can impersonate any agent. No access control on channels.

**Solution:**
- `MURMUR_ADMIN_KEY` env var for admin operations
- `POST /keys` (admin) — generate an API key tied to an agent name
- `X-Murmur-Key` header required on all requests (except `/health`)
- Key validates that `sender` matches the key's agent
- Reject requests with missing/invalid keys (401)
- Store keys as bcrypt hashes in a `keys` table

**Migration:** Add `MURMUR_AUTH=optional` mode first. Logs warnings for unauthenticated requests but doesn't block. Flip to `required` once all agents have keys.

**Impact:** Prevents impersonation. Required foundation for any multi-tenant or external-facing use.

## Priority 3: Message TTL & Cleanup

**Problem:** Messages grow forever. No archival, no expiry. Postgres will slow down over months.

**Solution:**
- `MURMUR_MESSAGE_TTL=7d` env var (default: 7 days)
- Background goroutine runs hourly, deletes messages older than TTL
- `POST /messages` accepts optional `ttl` field (override per-message)
- `GET /messages` excludes expired messages
- Expired messages moved to `messages_archive` table before deletion (configurable)

**Impact:** Keeps the database lean. Prevents unbounded growth in long-running deployments.

## Priority 4: Agent Groups

**Problem:** Can't address "all sandboxes" or "all reviewers" without knowing each agent name. Adding a new sandbox requires updating every agent's instructions.

**Solution:**
- Add `groups` field to agents: `["sandbox", "deploy-targets"]`
- `to` field accepts group names prefixed with `@`: `"to": "@sandbox"`
- Message delivered to all agents in the group
- Agents self-assign groups on registration

**Impact:** Scales agent coordination beyond hardcoded names. Deploy requests go to `@deploy-targets` instead of `host`.

## Priority 5: Webhooks

**Problem:** External systems (Slack, CI/CD) can't receive Murmur messages without polling.

**Solution:**
- `POST /webhooks` — register a URL + channel filter
- Murmur POSTs message JSON to the URL on match
- Retry 3x with exponential backoff on failure
- `GET /webhooks` and `DELETE /webhooks/{id}` for management

**Impact:** Bridges Murmur to Slack notifications, CI triggers, monitoring dashboards.

## Implementation Order

```
1. Agent heartbeats    (~2 hours)  — immediate operational value
2. API keys            (~3 hours)  — security foundation
3. Message TTL         (~1 hour)   — operational hygiene
4. Agent groups        (~2 hours)  — scalability
5. Webhooks            (~3 hours)  — ecosystem integration
```

## What NOT to Build Yet

- **Redis/NATS backend** — Postgres is fine at current scale
- **Multi-tenancy/workspaces** — only one team using it
- **E2E encryption** — internal network, no sensitive data in messages
- **Message editing/reactions** — nice-to-have, not blocking anything
- **Full-text search** — `GET /messages?after=` with channel filter is sufficient

These become relevant when Murmur serves multiple teams or handles >50 agents.
