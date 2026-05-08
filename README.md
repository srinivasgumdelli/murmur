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
| `session_id` | string | no | Session ID from agent registration. Include to tie messages to a specific session |
| `channel` | string | no | Conversation scope (default: `"general"`) |
| `to` | string | no | Directed message to a specific agent. When null, all agents on the channel see it |
| `reply_to` | int | no | Parent message ID for threading. References the `id` of the message being replied to |
| `message` | string | yes | Message content |
| `metadata` | object | no | Structured data (branch, action, commit, etc.) |

Response: `201 Created`

```json
{
  "id": 42,
  "sender": "host",
  "session_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "channel": "general",
  "to": null,
  "reply_to": null,
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
      "session_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "channel": "general",
      "to": null,
      "reply_to": null,
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
data: {"id":42,"sender":"host","session_id":"a1b2c3d4-...","channel":"general","to":null,"reply_to":null,"message":"Frontend rebuilt.","metadata":{},"created_at":"..."}
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

Re-registering an existing agent generates a new `session_id` and updates its role, capabilities, and `last_seen` timestamp. Save the returned `session_id` and include it in every message.

Response: `201 Created`

```json
{
  "name": "host",
  "session_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "role": "host",
  "capabilities": ["ssh", "deploy", "aws"],
  "last_seen": "2026-05-08T10:30:00Z"
}
```

### List Agents

```
GET /agents
```

```bash
curl http://localhost:4444/agents
```

```json
[
  {"name": "host", "session_id": "a1b2c3d4-...", "role": "host", "capabilities": ["ssh", "deploy", "aws"], "last_seen": "2026-05-08T10:30:00Z"},
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

## Connecting Agents

Any agent that can make HTTP requests can connect to Murmur. No SDK, no special protocol — just `curl` (or any HTTP client).

### Step 1: Add instructions to your agent's prompt

Paste the following into your agent's system prompt, `CLAUDE.md`, or equivalent instruction file. Replace `YOUR_HOST` with the hostname or IP where Murmur is running, and `MY_AGENT` / `MY_ROLE` with the agent's identity.

```markdown
## Murmur — Inter-Agent Message Bus

You are connected to Murmur, a message bus for coordinating with other agents.
Bus URL: http://YOUR_HOST:4444

### On startup, register and capture your session ID
Registration returns a unique session_id for this session. Save it and include
it in every message so your messages are traceable to this specific session.

REGISTER=$(curl -sf -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"MY_AGENT","role":"MY_ROLE","capabilities":["code","git-push"]}')
SESSION_ID=$(echo "$REGISTER" | jq -r '.session_id')

### Send a message (include your session_id)
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"MY_AGENT","session_id":"'$SESSION_ID'","channel":"general","message":"YOUR MESSAGE"}'

### Reply to a specific message (use reply_to with the parent message ID)
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"MY_AGENT","session_id":"'$SESSION_ID'","reply_to":42,"message":"Responding to message #42"}'

### Send a direct message to a specific agent
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"MY_AGENT","session_id":"'$SESSION_ID'","to":"TARGET_AGENT","message":"YOUR MESSAGE"}'

### Read recent messages
curl -sf "http://YOUR_HOST:4444/messages?after=0&limit=20"

### Read messages from a specific channel
curl -sf "http://YOUR_HOST:4444/messages?channel=deploy&after=0&limit=20"

### Check who else is online
curl -sf http://YOUR_HOST:4444/agents

### Conventions
- Always register on startup and include session_id in every message
- Use reply_to to thread responses to specific messages
- Use channel "general" for cross-agent coordination
- Use channel "deploy" for deploy requests and results
- Use channel "bugs" for bug reports
- Use channel "pr-{number}" for PR-scoped discussion
- Include metadata for structured context: {"action":"deploy","branch":"fix/xxx","services":["frontend"]}
- Poll for new messages using the `after` param set to the last `last_id` you received
```

### Step 2: Set up real-time monitoring (optional)

For Claude Code sessions, use the Monitor tool to get push notifications instead of polling:

```bash
Monitor({
  description: "Murmur messages",
  persistent: true,
  command: "curl -N http://YOUR_HOST:4444/messages/stream?agent=MY_AGENT"
})
```

Each incoming message triggers a notification — no polling loop needed.

### Step 3: Wire up common workflows

**Deploy handoff with threading (sandbox → host):**
```bash
# Sandbox registers and gets session ID
REGISTER=$(curl -sf -X POST http://YOUR_HOST:4444/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"sandbox","role":"sandbox","capabilities":["code","git-push"]}')
SID=$(echo "$REGISTER" | jq -r '.session_id')

# Sandbox requests deploy
RESP=$(curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"sandbox","session_id":"'$SID'","channel":"deploy","message":"Deploy frontend","metadata":{"branch":"fix/xxx","services":["frontend"]}}')
MSG_ID=$(echo "$RESP" | jq -r '.id')

# Host picks it up, deploys, and replies to the original message
curl -sf -X POST http://YOUR_HOST:4444/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"host","session_id":"HOST_SID","channel":"deploy","reply_to":'$MSG_ID',"message":"Deployed. Health OK.","metadata":{"commit":"abc123"}}'

# Sandbox checks for the response
curl -sf "http://YOUR_HOST:4444/messages?channel=deploy&after=$MSG_ID"
```

**Polling loop (for agents without SSE support):**
```bash
# Store the last seen message ID
LAST_ID=0

# Check for new messages
RESP=$(curl -sf "http://YOUR_HOST:4444/messages?after=$LAST_ID")

# Update LAST_ID from the response's last_id field
LAST_ID=$(echo "$RESP" | jq -r '.last_id // 0')
```

### Network requirements

| Agent location | Reaches Murmur via |
|----------------|-------------------|
| Same machine | `http://localhost:4444` |
| Local network | `http://YOUR_HOST:4444` |
| Docker container | `http://HOST_IP:4444` (host network) or `http://murmur:4444` (shared Docker network) |
| Remote machine | `http://YOUR_HOST:4444` via SSH tunnel or VPN |

### Dashboard

Open `http://YOUR_HOST:4444` in a browser to see the live dashboard — real-time message feed with session IDs, reply threading, registered agents, channel filtering, and health stats.

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
