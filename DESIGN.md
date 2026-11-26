# Murmur — Message Bus and Project Memory

## What

A lightweight message bus and persistent project memory for AI agent sessions. Messages support live conversations over channels. Memory supports searching for durable project knowledge across sessions.

## Why

AI agent systems that span multiple sessions (host + sandbox, reviewer + builder, orchestrator + specialists) need a reliable communication channel. Every existing approach has problems:

- **File-based handoff**: Race conditions, no history, status confusion, fragile polling
- **Orchestrator dispatch**: Central bottleneck, no peer-to-peer, sequential only
- **Search-only handoff**: Agents have to look for updates during an active conversation

Murmur delivers agent messages with history. Its memory API stores searchable notes with verified authorship, scoped version history, and lead or admin promotion.

## Architecture

```mermaid
flowchart LR
    E[Trusted enrollment container] -->|Admin enrollment| M[Murmur server]
    E -->|Save agent credential| V[Private agent volume]
    V -->|Read-only mount| A[Agent container]
    A -->|MCP memory and HTTP messages| M
    M --> P[(Postgres)]
```

Messages, agents, and API keys use the dedicated Compose Postgres instance. Memory uses that database by default and can use a local SQLite file. The same binary runs as the server and as the launcher in agent images. The memory service applies the same permissions, limits, and events across HTTP and MCP. Claude Code hooks use the HTTP API for session context and command capture.

## Endpoints

### `POST /messages`

Send a message.

```json
{
  "sender": "host",
  "channel": "general",
  "to": null,
  "message": "Frontend rebuilt and running. CSP nonces working.",
  "metadata": {
    "branch": "fix/frontend-rebuild",
    "action": "deploy",
    "services": ["frontend"]
  }
}
```

- `sender` (required): agent name
- `channel` (optional, default: `"general"`): conversation scope
- `to` (optional): directed message to a specific agent. When set, only that agent sees it in their stream. When null, all agents on the channel see it.
- `message` (required): the content
- `metadata` (optional): structured data (branch, action, commit, etc.)

Response: `201 Created`
```json
{
  "id": 42,
  "sender": "host",
  "channel": "general",
  "to": null,
  "message": "Frontend rebuilt and running. CSP nonces working.",
  "metadata": {"branch": "fix/frontend-rebuild", "action": "deploy"},
  "created_at": "2026-05-08T10:30:00Z"
}
```

### `GET /messages`

Fetch messages.

Query params:
- `channel` (optional, default: `"general"`): filter by channel
- `after` (optional): return messages with id > value (incremental reads)
- `limit` (optional, default: 50, max: 200): number of messages

Response: `200 OK`
```json
{
  "messages": [...],
  "last_id": 45
}
```

### `GET /messages/stream`

SSE stream. Holds connection open, pushes new messages in real-time via Postgres LISTEN/NOTIFY.

Query params:
- `channel` (optional, default: `"general"`): filter by channel
- `agent` (optional): filter to messages addressed to this agent (includes broadcasts)

Each event:
```
event: message
data: {"id":42,"sender":"host","channel":"general","message":"Frontend rebuilt.","metadata":{},"created_at":"..."}
```

Heartbeat every 30s:
```
event: heartbeat
data: {}
```

### `POST /agents`

Register an agent.

```json
{
  "name": "sandbox",
  "role": "code-writer",
  "description": "Working on frontend rebuild",
  "capabilities": ["code", "git-push"],
  "groups": ["sandbox"]
}
```

### `GET /agents`

List registered agents.

```json
[
  {"name": "host", "session_id": "a1b2c3d4-...", "role": "host", "capabilities": ["ssh", "deploy"], "groups": ["deploy-targets"], "status": "online", "last_seen": "2026-05-08T10:30:00Z"},
  {"name": "sandbox", "session_id": "b2c3d4e5-...", "role": "code-writer", "description": "Working on frontend rebuild", "capabilities": ["code", "git-push"], "groups": ["sandbox"], "status": "online", "last_seen": "2026-05-08T10:29:00Z"}
]
```

### Agent enrollment and identity

| Endpoint | Behavior |
| --- | --- |
| `POST /keys` | Requires the admin key. Returns a new agent key once and stores its hash. Send `{"agent":"reviewer","enroll":true}` to create the agent if needed and issue its key in one transaction. Enrollment preserves existing agent metadata. |
| `GET /identity` | Requires a valid Murmur key in every auth mode. Returns the verified agent identity and whether the key has admin or lead access. |
| `POST /agents/{name}/heartbeat?inbox=false` | Records launcher presence and returns the agent without fetching or changing the delivery status of pending messages. |

For Docker, the enrollment container writes the agent credential into a volume dedicated to that identity. The regular agent mounts it read-only. The launcher selects the profile by bus URL and agent name, verifies the credential, and supplies it to the MCP client and optional hooks or watcher. Read the [Docker enrollment guide](docs/agent-enrollment.md) for commands.

### `GET /health`

```json
{"status": "ok", "messages": 142, "agents": 2, "uptime": "2h30m", "memory": "ok"}
```

Memory availability is checked only with `MURMUR_AUTH=required`. A failed memory check returns HTTP 503 and a `degraded` status.

### Memory

Memory endpoints require `MURMUR_AUTH=required` and a valid key in `X-Murmur-Key` or an `Authorization: Bearer` header. Scopes group notes by workspace, task, or shared project knowledge. The API limits note bodies to 4096 bytes.

| Endpoint | Behavior |
|---|---|
| `POST /memories` | Save a reported entry in a workspace or task scope. The server sets authorship from the key. Returns HTTP 201 for a new version or 200 for a reused capture. |
| `GET /memories` | Search current entries with `q`, `scope`, `tags`, `kind`, `source`, `author`, `session_id`, `since`, `include_history`, and `limit`. Repeat `tags` or use comma-separated values. Percent-encode any comma within a tag. `X-Murmur-Session-ID` associates the search with a session for metrics. The response includes items and a total before the limit. |
| `GET /memories/{id}` | Read an entry, with its superseded versions when `include_history=true`. |
| `POST /memories/{id}/promote` | Copy an entry into shared memory. Requires a lead or admin. Returns HTTP 200 when reusing a keyless promotion. |
| `DELETE /memories/{id}` | Expire an entry. Requires the author, a lead, or an admin. Promoted entries require a lead or admin. |
| `POST /memories/{id}/purge` | Remove one entry and log the action. Requires an admin. |
| `GET /memories/stats` | Return scope counts and seven-day activity metrics. |
| `GET /messages/stream?types=memory` | Subscribe to memory events on a dedicated stream. Use `scope` to filter by event scope or promotion origin. Events include the action and cover writes, promotions, expiries, and purges made through the local Murmur instance. |
| `/mcp` | Streamable HTTP MCP endpoint for `memory_search`, `memory_put`, and `memory_expire`. |

The author, a lead, or an admin can supersede a current keyed entry. Promotions track the original scope, so the same key can appear in shared memory from different scopes. Search uses Postgres full-text search or SQLite FTS5, with provenance and recency to break ties. Expiry removes an entry from normal search. Reported entries have a default 90-day TTL, and the reaper removes expired history after 30 days by default.

See the [memory design](docs/specs/2026-11-26-memory-design.md) for the data model, trust rules, and rollout.

## Database

Dedicated Postgres instance via Docker Compose. Not shared with any project database.

### Schema (auto-applied on startup)

```sql
CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,
    sender TEXT NOT NULL,
    channel TEXT NOT NULL DEFAULT 'general',
    "to" TEXT,
    message TEXT NOT NULL,
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages (channel, id);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages (created_at);

CREATE TABLE IF NOT EXISTS agents (
    name TEXT PRIMARY KEY,
    role TEXT NOT NULL,
    capabilities TEXT[] DEFAULT '{}',
    last_seen TIMESTAMPTZ DEFAULT now()
);

-- LISTEN/NOTIFY for real-time SSE
CREATE OR REPLACE FUNCTION notify_new_message()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('new_message', json_build_object(
        'id', NEW.id,
        'channel', NEW.channel,
        'to', NEW."to"
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS message_inserted ON messages;
CREATE TRIGGER message_inserted
    AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION notify_new_message();
```

## Project Structure

```
murmur/
├── cmd/murmur/main.go          # Entrypoint: config, wiring, server start
├── internal/
│   ├── handler/
│   │   ├── messages.go         # POST/GET /messages
│   │   ├── stream.go           # GET /messages/stream (SSE)
│   │   ├── agents.go           # POST/GET /agents
│   │   └── health.go           # GET /health
│   ├── model/
│   │   └── model.go            # Message and Agent types
│   └── schema/
│       └── schema.go           # DDL and auto-migration
├── go.mod
├── go.sum
├── Makefile                    # build, run, docker, lint, test
├── Dockerfile                  # Multi-stage distroless build
├── docker-compose.yml          # Murmur + dedicated Postgres
└── DESIGN.md                   # This file
```

Standard Go project layout with `cmd/` entrypoint and `internal/` packages.

## Configuration

Environment variables:
```bash
BUS_PORT=4444                    # HTTP server port
BUS_DATABASE_URL=postgres://murmur:murmur@postgres:5432/murmur?sslmode=disable
MURMUR_ADMIN_KEY=                # Admin key for key management (empty = key management disabled)
MURMUR_AUTH=off                  # Auth mode: off, optional, required
MURMUR_MESSAGE_TTL=              # Message retention (e.g. '7 days'). Empty = keep forever
MURMUR_MEMORY_STORE=postgres     # Memory backend: postgres or sqlite
MURMUR_MEMORY_DB=./murmur-memory.db # SQLite memory database file
MURMUR_MEMORY_TTL_DAYS=90         # Default lifetime for reported entries
MURMUR_MEMORY_KEEP_EXPIRED_DAYS=30 # Expired-history retention
MURMUR_MEMORY_SCOPE_CAP=1000      # Current entries per scope
```

## Docker Compose

The [Compose configuration](docker-compose.yml) starts Murmur and Postgres, keeps Postgres data in `murmur-data`, and mounts `murmur-memory-data` at the container's writable home directory. Set `MURMUR_AUTH=required` to enable memory and `MURMUR_ADMIN_KEY` to administer enrollment and promotion. The [agent Dockerfile](Dockerfile.agent) adds the launcher to your runtime image. Its credential volumes are separate from the server volumes. Set `MURMUR_MEMORY_STORE=sqlite` to use the mounted volume for memory. Override `MURMUR_MEMORY_DB` to select a different file inside the container.

## Dockerfile

```dockerfile
FROM golang:1.26.3-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/murmur ./cmd/murmur

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/murmur /murmur
EXPOSE 4444
ENTRYPOINT ["/murmur"]
```

## Deployment

Clone the repo and run:
```bash
cd ~/murmur
docker compose up -d
```

That's it. Own Postgres, own container, no coupling to other services.

Agents connect via `http://YOUR_HOST:4444` (from local network) or `http://localhost:4444` (from YOUR_HOST itself).

## Usage

### From Claude Code sessions (curl)

```bash
# Send
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","message":"Deployed frontend."}'

# Read
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") "http://YOUR_HOST:4444/messages?after=0&limit=10"

# Long poll (recommended for agents) — after=latest starts at the current tip
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") "http://YOUR_HOST:4444/messages/poll?agent=host&after=latest&timeout=30"

# Stream (dashboard only)
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -N http://YOUR_HOST:4444/messages/stream

# Register
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"host","role":"host","capabilities":["ssh","deploy"]}'
```

### MCP server

The launcher configures a Streamable HTTP connection to `/mcp` with the agent key. Chat, presence, and the watcher use the existing HTTP endpoints. Agents and watchers share the enrolled identity.

## Message Conventions

Not enforced by the server, but recommended:

```json
// Deploy request
{"sender": "sandbox", "channel": "deploy", "message": "Deploy frontend", "metadata": {"action": "deploy", "branch": "fix/xxx", "services": ["frontend"]}}

// Deploy result
{"sender": "host", "channel": "deploy", "message": "Deployed. Health OK.", "metadata": {"action": "deploy-result", "commit": "abc123"}}

// Bug report
{"sender": "host", "channel": "bugs", "message": "Resource log margins off", "metadata": {"action": "bug"}}

// Direct question
{"sender": "sandbox", "to": "host", "message": "Is YOUR_HOST reachable?"}
```

## Channels

Channels scope conversations. No explicit creation required; posting to a channel creates it implicitly.

Recommended channels:
- `general` — default, cross-agent coordination
- `deploy` — deploy requests and results
- `bugs` — bug reports and fixes
- `pr-{number}` — discussion scoped to a PR

## Multi-Agent Patterns

**2 agents (MVP)**: host + sandbox on `general` channel. Direct replacement for handoff.md.

**3-5 agents**: Add channels for separation. `deploy` for host, `review` for reviewer, `general` for coordination.

**Hub-and-spoke**: Orchestrator posts tasks to directed messages, specialists respond.

**Peer-to-peer**: Agents talk directly via channels without a central coordinator.

**Broadcast**: Post to `general` without `to` field. All agents see it.

## Scaling (3-8 agents)

| Concern | Impact | Notes |
|---------|--------|-------|
| SSE connections | 3-8 open | Trivial for Go |
| Message volume | ~500/hour | Postgres handles millions |
| LISTEN/NOTIFY | All listeners notified | Client-side channel filter |
| History | Grows linearly | Add TTL/archival if needed |

## Security

- API key authentication via `X-Murmur-Key` header
- Three auth modes: `off` (default), `optional` (logs warnings), `required` (blocks)
- `MURMUR_ADMIN_KEY` for key generation and admin access
- Don't send secrets/credentials through the bus
- Postgres not exposed outside Docker network

## Ecosystem Gap

No existing solution provides agent-to-agent chat:

| Project | Stars | What it does | Real-time chat? |
|---------|-------|-------------|-----------------|
| MemPalace | 51K | Semantic memory search | No |
| claude-pipeline | 112 | Skill orchestration | No |
| big-3-super-agent | 295 | Voice dispatcher | No |
| MCP server-memory | — | Knowledge graph | No |
| Google MCP Toolbox | 15K | Database connector | No |

Murmur fills this gap.

## Agent Connection Guide

### How agents connect for messaging

Use the [Docker enrollment guide](docs/agent-enrollment.md) to enroll and launch a containerized agent. The following HTTP examples show how clients can send and read messages directly.

### For host session (CLAUDE.local.md)

```markdown
## Murmur

Message bus for inter-session communication. Use curl via Bash tool.

Bus URL: http://YOUR_HOST:4444

### Send a message
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","message":"YOUR MESSAGE","metadata":{}}'

### Read recent messages
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://YOUR_HOST:4444/messages?after=0&limit=20"

### Read new messages since last check
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://YOUR_HOST:4444/messages?after=LAST_ID"

### Long poll for messages (recommended for real-time)
# Server holds request up to 30s, returns instantly on new messages
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://YOUR_HOST:4444/messages/poll?agent=host&after=LAST_ID&timeout=30"

### Register on startup
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"host","role":"host","capabilities":["ssh","deploy","aws","e2e"]}'
```

### For sandbox session (moat CLAUDE.md or agent prompt)

```markdown
## Murmur

Message bus for inter-session communication. Use curl via Bash tool.

Bus URL: http://BUS_HOST:4444

### Send a message
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://BUS_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","message":"YOUR MESSAGE","metadata":{}}'

### Read recent messages
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://BUS_HOST:4444/messages?after=0&limit=20"

### Check for replies (poll pattern)
LAST_ID=0
RESP=$(curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://BUS_HOST:4444/messages?after=$LAST_ID")
# Parse RESP, update LAST_ID from last_id field

### Register on startup
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://BUS_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"sandbox","role":"sandbox","capabilities":["code","git-push"]}'
```

Replace `BUS_HOST` with YOUR_HOST's IP or hostname reachable from the sandbox container.

### Connection patterns

**Request-response (deploy handoff)**:
```bash
# Sandbox sends
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://BUS_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","channel":"deploy","message":"Deploy frontend please","metadata":{"branch":"fix/xxx","services":["frontend"]}}'

# Host reads deploy channel
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://YOUR_HOST:4444/messages?channel=deploy&after=0"

# Host responds
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","channel":"deploy","message":"Deployed. Health OK.","metadata":{"commit":"abc123"}}'

# Sandbox checks for reply
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://BUS_HOST:4444/messages?channel=deploy&after=LAST_ID"
```

**Real-time monitoring (host runs a background watcher)**:
```javascript
// In Claude Code, run murmur-wait.sh (see README, "Start the background
// message watcher") as a background Bash command. It long-polls with
// after=<last_id>&timeout=30, exits when a message from another agent
// arrives, and is restarted after each one.
Bash({ command: "./murmur-wait.sh", run_in_background: true })
```
Each new message ends the watcher and triggers one notification. Long poll holds for 30s and returns instantly on new messages. The Monitor tool is not used because it caps watches at 30 minutes.

**Direct message (1:1)**:
```bash
# Sandbox asks host directly
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf -X POST http://BUS_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","to":"host","message":"Is the RDS still alive?"}'

# Host long-polls for messages addressed to it
curl -H @<(printf 'X-Murmur-Key: %s\n' "${MURMUR_KEY:-}") -sf "http://YOUR_HOST:4444/messages/poll?agent=host&after=latest&timeout=30"
```

### Network requirements

| Agent | Location | Reaches bus via |
|-------|----------|----------------|
| Host Claude Code | Local machine | `http://YOUR_HOST:4444` (SSH tunnel or LAN) |
| Sandbox (moat) | Docker container | `http://HOST_IP:4444` (outbound HTTP) |
| Same Docker network | Docker network | `http://murmur:4444` |
| Future agents | Any machine with HTTP | `http://YOUR_HOST:4444` (LAN) or via tunnel |

Use the Compose service address for container-to-container HTTP and MCP. On the Docker host, the published port is available at `http://localhost:4444`. A Docker Desktop connection through `host.docker.internal` can be forwarded to a loopback listener, where MCP checks the Host header.

## Build

```bash
cd ~/Repos/murmur
go build -o murmur .

# Or via Docker
docker compose up -d --build
```

## Timeline

- 10 min: main.go with pgx connection + auto-migration
- 15 min: POST /messages + GET /messages handlers
- 15 min: SSE stream with LISTEN/NOTIFY
- 5 min: Agent registration endpoints
- 5 min: Dockerfile + docker-compose.yml
- 5 min: Deploy to YOUR_HOST + test
