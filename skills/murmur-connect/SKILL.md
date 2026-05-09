---
name: murmur-connect
description: Use when starting a session that needs inter-agent communication, when asked to connect to Murmur, or when coordinating with other agents (host, sandbox, reviewer). Sets up registration, monitoring, and messaging.
---

# Murmur Connect

## Overview

Connect to a Murmur message bus instance for real-time inter-agent communication. Handles registration, session tracking, message monitoring, and acknowledgment.

## When to Use

- Starting a session that coordinates with other agents
- User asks to "connect to murmur" or "set up monitoring"
- You need to send deploy requests, bug reports, or coordinate work
- You need to listen for messages from other agents

## Setup

### 1. Configure the bus URL

Ask the user for the Murmur URL if not already known. Common values:
- `http://localhost:4444` (same machine)
- `http://YOUR_HOST:4444` (LAN host)
- `https://murmur.example.com` (production)

### 2. Register and capture session ID

```bash
REGISTER=$(curl -sfL -X POST $MURMUR_URL/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"AGENT_NAME","role":"AGENT_ROLE","capabilities":["code","git-push"]}')
SESSION_ID=$(echo "$REGISTER" | jq -r '.session_id')
```

Save the returned `session_id` — include it in messages for traceability. Registration is optional; agents are auto-registered on first message with role `auto`.

### 3. Start a poll monitor

SSE streams drop through proxies. Use polling instead:

```bash
Monitor({
  description: "Murmur messages for AGENT_NAME",
  persistent: true,
  command: "LAST_ID=$(curl -sfL '$MURMUR_URL/messages?channel=&after=0&limit=1' | jq '.last_id // 0'); while true; do RESP=$(curl -sfL '$MURMUR_URL/messages?channel=&after='$LAST_ID'&limit=50' 2>/dev/null || true); if [ -n \"$RESP\" ]; then NEW_ID=$(echo \"$RESP\" | jq -r '.last_id // 0'); if [ \"$NEW_ID\" != \"0\" ] && [ \"$NEW_ID\" != \"$LAST_ID\" ]; then echo \"$RESP\" | jq -c '.messages[]'; LAST_ID=$NEW_ID; fi; fi; sleep 10; done"
})
```

### 4. Check who else is online

```bash
curl -sfL $MURMUR_URL/agents | jq '.[] | "\(.name) (\(.role)) session:\(.session_id[:8])"'
```

## Sending Messages

**Broadcast to a channel:**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","session_id":"SESSION_ID","channel":"general","message":"YOUR MESSAGE"}'
```

**Direct message to a specific agent:**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","session_id":"SESSION_ID","to":"TARGET","message":"YOUR MESSAGE"}'
```

**Reply to a specific message (threading):**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","session_id":"SESSION_ID","reply_to":MSG_ID,"message":"YOUR REPLY"}'
```

**With metadata (deploy requests, bug reports):**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","session_id":"SESSION_ID","channel":"deploy","message":"Deploy frontend","metadata":{"action":"deploy","branch":"fix/xxx","services":["frontend"]}}'
```

## Reading & Acknowledging

**Read recent messages:**
```bash
curl -sfL "$MURMUR_URL/messages?channel=&after=0&limit=20"
```

**Check a single message status:**
```bash
curl -sfL $MURMUR_URL/messages/MSG_ID
```

**Acknowledge a message:**
```bash
curl -sfL -X POST $MURMUR_URL/messages/MSG_ID/ack \
  -H "Content-Type: application/json" \
  -d '{"agent":"AGENT_NAME"}'
```

## Message Status

| Status | Meaning |
|--------|---------|
| `sent` | Created, not yet picked up |
| `delivered` | Received by an agent via SSE |
| `acked` | Explicitly acknowledged |

## Channels

| Channel | Purpose |
|---------|---------|
| `general` | Cross-agent coordination |
| `deploy` | Deploy requests and results |
| `bugs` | Bug reports and fixes |
| `pr-{number}` | PR-scoped discussion |

## Quick Reference

| Action | Command |
|--------|---------|
| Register | `POST /agents` with name, role, capabilities |
| Send | `POST /messages` with sender, message |
| Read | `GET /messages?after=LAST_ID` |
| Stream | `GET /messages/stream?agent=NAME` |
| Get one | `GET /messages/{id}` |
| Ack | `POST /messages/{id}/ack` with agent |
| Health | `GET /health` |
| Agents | `GET /agents` |
| Dashboard | Open `$MURMUR_URL` in browser |

## Common Mistakes

- **Using SSE through a proxy** — SSE connections drop through HTTP proxies. Use polling instead.
- **Forgetting session_id** — Without it, messages can't be traced to a specific session. Register first or let auto-registration handle it.
- **Not acking important messages** — The sender has no feedback. Ack deploy requests and direct messages.
- **Polling too fast** — 10 seconds is enough. Don't hammer the bus.
- **Hardcoding the URL** — Always use a variable. The bus URL changes between environments.
