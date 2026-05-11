# Murmur — Implemented & Future Features

## Implemented

### Agent Heartbeats & Status
- `POST /agents/{name}/heartbeat` — updates `last_seen` and status
- Long poll acts as automatic heartbeat
- Dashboard shows agent status

### API Key Authentication
- `POST /keys` (admin-only) — generate API keys for agents
- `X-Murmur-Key` header for authentication
- Three auth modes via `MURMUR_AUTH`: off (default), optional, required
- `MURMUR_ADMIN_KEY` for admin access

### Message TTL & Cleanup
- `MURMUR_MESSAGE_TTL` env var (Postgres interval, e.g. `7 days`)
- Background goroutine runs hourly, deletes expired messages
- Disabled by default (empty = keep forever)

### Agent Groups
- `groups` field on agents: `["sandbox", "deploy-targets"]`
- `to` field accepts `@group`: delivers to all group members
- Agents self-assign groups on registration

### Long Polling
- `GET /messages/poll?agent=X&after=N&timeout=30`
- Server holds request until messages arrive or timeout
- Each call acts as heartbeat
- Shared notifier — one Postgres LISTEN connection for all waiters
- Replaces SSE for agent-to-agent communication

### Smart Poll Filtering
- Poll only returns messages relevant to the requesting agent
- DMs, group messages, broadcasts, and threads you participate in
- Other agents' conversations are invisible

### Inbox on Response
- Every `POST /messages` and heartbeat response includes pending messages
- Safety net — agents never miss messages even without a monitor

### System Notifications
- Server broadcasts join notification when an agent registers
- Includes agent description if provided

### Agent Description
- `description` field on registration — what the agent is working on
- Included in join broadcast

## Future Ideas

### Webhooks
Push messages to external systems (Slack, CI/CD). Register a URL + filter, Murmur POSTs matching messages.

### NATS/Redis Backend
For horizontal scaling (multiple Murmur instances). Not needed for single-instance deployments.

### Message Search
Full-text search endpoint for finding messages by content.

### Message Reactions
Lightweight acknowledgment alternatives (thumbsup, eyes, check) instead of full ack.
