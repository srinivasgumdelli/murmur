# Murmur

A lightweight message bus for AI agent sessions to communicate reliably. Agents post messages, read messages, and stream updates in real-time. Channels scope conversations. Postgres stores history. SSE delivers messages instantly.

## Why

AI agent systems that span multiple sessions (host + sandbox, reviewer + builder, orchestrator + specialists) need a reliable communication channel. Existing approaches fall short:

- **File-based handoff** — race conditions, no history, fragile polling
- **Orchestrator dispatch** — central bottleneck, no peer-to-peer, sequential only
- **Shared memory/knowledge graph** — wrong abstraction (search, not chat)

Murmur is a message bus. Agents decide what to say and who to talk to. Murmur just delivers messages reliably with history.

## Architecture

```mermaid
graph TD
    A["Agent A<br/>(host/SSH)"] -- HTTP --> Bus["Murmur<br/>:4444"]
    Bus -- SSE --> A

    B["Agent B<br/>(sandbox)"] -- HTTP --> Bus
    Bus -- SSE --> B

    C["Agent C<br/>(reviewer)"] -- HTTP --> Bus
    Bus -- SSE --> C

    D["Agent D<br/>(tester)"] -- HTTP --> Bus
    Bus -- SSE --> D

    Bus --> DB[("Postgres<br/>(own DB)")]
```

Single Go binary. Dedicated Postgres instance via Docker Compose (not shared with any project database). Distroless container image.

## Quick Start

### Docker Compose (recommended)

```bash
git clone https://github.com/srinivasgumdelli/murmur.git
cd murmur
docker compose up -d
```

The bus is now running at `http://localhost:4444`. Postgres is included — no external dependencies.

### From Source

```bash
# Requires Go 1.26+ and a running Postgres instance
make build

export BUS_DATABASE_URL="postgres://murmur:murmur@localhost:5432/murmur?sslmode=disable"
export BUS_PORT=4444
./murmur
```

Schema is applied automatically on startup.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `BUS_PORT` | `4444` | HTTP server port |
| `BUS_DATABASE_URL` | `postgres://murmur:murmur@localhost:5432/murmur?sslmode=disable` | Postgres connection string |

## API

### Send a Message

```
POST /messages
```

```bash
curl -X POST http://localhost:4444/messages \
  -H "Content-Type: application/json" \
  -d '{
    "sender": "host",
    "channel": "general",
    "message": "Frontend rebuilt and running.",
    "metadata": {"branch": "fix/frontend-rebuild", "action": "deploy"}
  }'
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sender` | string | yes | Agent name |
| `channel` | string | no | Conversation scope (default: `"general"`) |
| `to` | string | no | Directed message to a specific agent. When null, all agents on the channel see it |
| `message` | string | yes | Message content |
| `metadata` | object | no | Structured data (branch, action, commit, etc.) |

Response: `201 Created`

```json
{
  "id": 42,
  "sender": "host",
  "channel": "general",
  "to": null,
  "message": "Frontend rebuilt and running.",
  "metadata": {"branch": "fix/frontend-rebuild", "action": "deploy"},
  "created_at": "2026-05-08T10:30:00Z"
}
```

### Read Messages

```
GET /messages
```

```bash
curl "http://localhost:4444/messages?channel=general&after=0&limit=20"
```

| Param | Default | Description |
|-------|---------|-------------|
| `channel` | `"general"` | Filter by channel |
| `after` | `0` | Return messages with id > value (incremental reads) |
| `limit` | `50` (max: 200) | Number of messages to return |

Response: `200 OK`

```json
{
  "messages": [
    {
      "id": 1,
      "sender": "host",
      "channel": "general",
      "to": null,
      "message": "Frontend rebuilt and running.",
      "metadata": {},
      "created_at": "2026-05-08T10:30:00Z"
    }
  ],
  "last_id": 1
}
```

### Stream Messages (SSE)

```
GET /messages/stream
```

```bash
curl -N "http://localhost:4444/messages/stream?channel=general&agent=host"
```

| Param | Default | Description |
|-------|---------|-------------|
| `channel` | `"general"` | Filter by channel |
| `agent` | — | Only receive messages addressed to this agent (plus broadcasts) |

Holds the connection open. New messages arrive as SSE events via Postgres LISTEN/NOTIFY:

```
event: message
data: {"id":42,"sender":"host","channel":"general","message":"Frontend rebuilt.","metadata":{},"created_at":"..."}
```

Heartbeat every 30 seconds:

```
event: heartbeat
data: {}
```

### Register an Agent

```
POST /agents
```

```bash
curl -X POST http://localhost:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name": "host", "role": "host", "capabilities": ["ssh", "deploy", "aws"]}'
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique agent name |
| `role` | string | yes | Agent role (host, sandbox, reviewer, etc.) |
| `capabilities` | string[] | no | What this agent can do |

Re-registering an existing agent updates its role, capabilities, and `last_seen` timestamp.

### List Agents

```
GET /agents
```

```bash
curl http://localhost:4444/agents
```

```json
[
  {"name": "host", "role": "host", "capabilities": ["ssh", "deploy", "aws"], "last_seen": "2026-05-08T10:30:00Z"},
  {"name": "sandbox", "role": "sandbox", "capabilities": ["code", "git-push"], "last_seen": "2026-05-08T10:29:00Z"}
]
```

### Health Check

```
GET /health
```

```bash
curl http://localhost:4444/health
```

```json
{"status": "ok", "messages": 142, "agents": 2, "uptime": "2h30m"}
```

## Channels

Channels scope conversations. No explicit creation required — posting to a channel creates it implicitly.

| Channel | Purpose |
|---------|---------|
| `general` | Default, cross-agent coordination |
| `deploy` | Deploy requests and results |
| `bugs` | Bug reports and fixes |
| `pr-{number}` | Discussion scoped to a PR |

## Multi-Agent Patterns

### Two Agents (MVP)

Host + sandbox on the `general` channel. Direct replacement for file-based handoff.

```bash
# Sandbox requests a deploy
curl -X POST http://bus:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","channel":"deploy","message":"Deploy frontend","metadata":{"branch":"fix/xxx","services":["frontend"]}}'

# Host picks it up and responds
curl -X POST http://bus:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","channel":"deploy","message":"Deployed. Health OK.","metadata":{"commit":"abc123"}}'
```

### Direct Messages (1:1)

```bash
# Sandbox asks host directly
curl -X POST http://bus:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","to":"host","message":"Is the RDS still alive?"}'

# Host streams only messages addressed to it
curl -N "http://bus:4444/messages/stream?agent=host"
```

### Hub-and-Spoke

Orchestrator posts tasks via directed messages, specialists respond on channels.

### Peer-to-Peer

Agents talk directly via channels without a central coordinator.

## Connecting from Claude Code

Add this to your session's CLAUDE.md:

```markdown
## Murmur

Bus URL: http://YOUR_HOST:4444

### Send a message
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"MY_AGENT_NAME","message":"YOUR MESSAGE","metadata":{}}'

### Read recent messages
curl -sf http://YOUR_HOST:4444/messages?after=0&limit=20

### Register on startup
curl -sf -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"MY_AGENT_NAME","role":"MY_ROLE","capabilities":["code","git-push"]}'
```

For real-time monitoring, use the Claude Code Monitor tool:

```bash
Monitor({
  description: "Bus messages",
  persistent: true,
  command: "curl -N http://YOUR_HOST:4444/messages/stream"
})
```

## Scaling

| Concern | At 3-8 agents | Notes |
|---------|---------------|-------|
| SSE connections | 3-8 open | Trivial for Go |
| Message volume | ~500/hour | Postgres handles millions |
| LISTEN/NOTIFY | All listeners notified | Client-side channel filter |
| History | Grows linearly | Add TTL/archival if needed |

## Security

- No authentication in MVP (designed for local network / internal use)
- Postgres is not exposed outside the Docker network
- Don't send secrets or credentials through the bus
- Add `X-Bus-Token` shared secret header if needed for your setup

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
├── DESIGN.md                   # Design document
└── README.md                   # This file
```

## License

MIT
