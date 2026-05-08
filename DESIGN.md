# Murmur — Message Bus for Agentic Systems

## What

A lightweight message bus for AI agent sessions to communicate reliably. Agents post messages, read messages, and stream updates in real-time. Channels scope conversations. Postgres stores history. SSE delivers messages instantly.

## Why

AI agent systems that span multiple sessions (host + sandbox, reviewer + builder, orchestrator + specialists) need a reliable communication channel. Every existing approach has problems:

- **File-based handoff**: Race conditions, no history, status confusion, fragile polling
- **Orchestrator dispatch**: Central bottleneck, no peer-to-peer, sequential only
- **Shared memory/knowledge graph**: Wrong abstraction (search, not chat)

Murmur is a message bus. Agents decide what to say and who to talk to. The bus just delivers messages reliably with history.

## Architecture

```
┌──────────────┐         ┌──────────────┐         ┌──────────────┐
│   Agent A     │──HTTP──▶│              │◀──HTTP──│   Agent B     │
│  (host/SSH)   │◀──SSE───│ Murmur  │───SSE──▶│  (sandbox)    │
└──────────────┘         │    :4444     │         └──────────────┘
                          │              │
┌──────────────┐         │              │         ┌──────────────┐
│   Agent C     │──HTTP──▶│              │◀──HTTP──│   Agent D     │
│  (reviewer)   │◀──SSE───│              │───SSE──▶│  (tester)     │
└──────────────┘         └──────┬───────┘         └──────────────┘
                                │
                          ┌─────▼─────┐
                          │ Postgres  │
                          │  (own DB) │
                          └───────────┘
```

Dockerized. Own Postgres instance, not shared with any project database.

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
  "name": "moat",
  "role": "sandbox",
  "capabilities": ["code", "git-push"]
}
```

### `GET /agents`

List registered agents with last-seen timestamps.

```json
[
  {"name": "host", "role": "host", "capabilities": ["ssh", "deploy", "aws"], "last_seen": "2026-05-08T10:30:00Z"},
  {"name": "moat", "role": "sandbox", "capabilities": ["code", "git-push"], "last_seen": "2026-05-08T10:29:00Z"}
]
```

### `GET /health`

```json
{"status": "ok", "messages": 142, "agents": 2, "uptime": "2h30m"}
```

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
```

## Docker Compose

```yaml
version: "3.8"

services:
  murmur:
    build: .
    ports:
      - "4444:4444"
    environment:
      - BUS_PORT=4444
      - BUS_DATABASE_URL=postgres://murmur:murmur@db:5432/murmur?sslmode=disable
    depends_on:
      db:
        condition: service_healthy
    restart: unless-stopped

  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: murmur
      POSTGRES_PASSWORD: murmur
      POSTGRES_DB: murmur
    volumes:
      - murmur-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U murmur"]
      interval: 5s
      timeout: 3s
      retries: 5
    restart: unless-stopped

volumes:
  murmur-data:
```

## Dockerfile

```dockerfile
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/murmur .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/murmur /murmur
EXPOSE 4444
ENTRYPOINT ["/murmur"]
```

## Deployment on YOUR_HOST

Clone the repo and run:
```bash
cd ~/murmur
docker compose up -d
```

That's it. Own Postgres, own container, no coupling to other-project.

Agents connect via `http://YOUR_HOST:4444` (from local network) or `http://localhost:4444` (from YOUR_HOST itself).

## Usage

### From Claude Code sessions (curl)

```bash
# Send
curl -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","message":"Deployed frontend."}'

# Read
curl http://YOUR_HOST:4444/messages?after=0&limit=10

# Stream
curl -N http://YOUR_HOST:4444/messages/stream

# Register
curl -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"host","role":"host","capabilities":["ssh","deploy"]}'
```

### Future: MCP Server

Wrap the HTTP API as an MCP server so agents get native tools (`send_message`, `read_messages`, `stream_messages`) instead of curl. HTTP-first is the right MVP. MCP is the upgrade path.

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

- No authentication in MVP (internal tool, local network only)
- Add `X-Bus-Token` shared secret header if needed
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

### How agents connect (no MCP, just curl via Bash tool)

Each Claude Code session has the Bash tool. Agents use `curl` to talk to the bus. Add the following to each session's CLAUDE.md or agent prompt.

### For host session (CLAUDE.local.md)

```markdown
## Murmur

Message bus for inter-session communication. Use curl via Bash tool.

Bus URL: http://YOUR_HOST:4444

### Send a message
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","message":"YOUR MESSAGE","metadata":{}}'

### Read recent messages
curl -sf http://YOUR_HOST:4444/messages?after=0&limit=20

### Read new messages since last check
curl -sf http://YOUR_HOST:4444/messages?after=LAST_ID

### Stream messages (use with Monitor tool for real-time)
Monitor: curl -N http://YOUR_HOST:4444/messages/stream

### Register on startup
curl -sf -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"host","role":"host","capabilities":["ssh","deploy","aws","e2e"]}'
```

### For sandbox session (moat CLAUDE.md or agent prompt)

```markdown
## Murmur

Message bus for inter-session communication. Use curl via Bash tool.

Bus URL: http://BUS_HOST:4444

### Send a message
curl -sf -X POST http://BUS_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","message":"YOUR MESSAGE","metadata":{}}'

### Read recent messages
curl -sf http://BUS_HOST:4444/messages?after=0&limit=20

### Check for replies (poll pattern)
LAST_ID=0
RESP=$(curl -sf http://BUS_HOST:4444/messages?after=$LAST_ID)
# Parse RESP, update LAST_ID from last_id field

### Register on startup
curl -sf -X POST http://BUS_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"sandbox","role":"sandbox","capabilities":["code","git-push"]}'
```

Replace `BUS_HOST` with YOUR_HOST's IP or hostname reachable from the sandbox container.

### Connection patterns

**Request-response (deploy handoff)**:
```bash
# Sandbox sends
curl -sf -X POST http://BUS_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","channel":"deploy","message":"Deploy frontend please","metadata":{"branch":"fix/xxx","services":["frontend"]}}'

# Host reads deploy channel
curl -sf http://YOUR_HOST:4444/messages?channel=deploy&after=0

# Host responds
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","channel":"deploy","message":"Deployed. Health OK.","metadata":{"commit":"abc123"}}'

# Sandbox checks for reply
curl -sf http://BUS_HOST:4444/messages?channel=deploy&after=LAST_ID
```

**Real-time monitoring (host uses Monitor tool)**:
```bash
# In Claude Code, use the Monitor tool with the SSE stream
Monitor({
  description: "Bus messages",
  persistent: true,
  command: "curl -N http://YOUR_HOST:4444/messages/stream"
})
```
Each new message triggers a notification. No polling needed.

**Direct message (1:1)**:
```bash
# Sandbox asks host directly
curl -sf -X POST http://BUS_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","to":"host","message":"Is the RDS still alive?"}'

# Host streams only messages addressed to it
curl -N "http://YOUR_HOST:4444/messages/stream?agent=host"
```

### Network requirements

| Agent | Location | Reaches bus via |
|-------|----------|----------------|
| Host Claude Code | Local machine | `http://YOUR_HOST:4444` (SSH tunnel or LAN) |
| Sandbox (moat) | Docker container | `http://HOST_IP:4444` (outbound HTTP) |
| YOUR_HOST services | Docker network | `http://localhost:4444` or `http://bus:4444` |
| Future agents | Any machine with HTTP | `http://YOUR_HOST:4444` (LAN) or via tunnel |

The bus only needs to be reachable via HTTP. No special protocols, no MCP, no WebSocket. Plain HTTP + SSE.

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
