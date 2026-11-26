package schema

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const ddl = `
CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,
    sender TEXT NOT NULL,
    session_id TEXT,
    channel TEXT NOT NULL DEFAULT 'general',
    "to" TEXT,
    reply_to INT,
    message TEXT NOT NULL,
    metadata JSONB DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'sent',
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages (channel, id);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages (created_at);
CREATE INDEX IF NOT EXISTS idx_messages_reply_to ON messages (reply_to);
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages ("to");

CREATE TABLE IF NOT EXISTS agents (
    name TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    description TEXT,
    capabilities TEXT[] DEFAULT '{}',
    groups TEXT[] DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'online',
    last_seen TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
    key_hash TEXT PRIMARY KEY,
    agent TEXT NOT NULL REFERENCES agents(name),
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS reserved_agent_names (name TEXT PRIMARY KEY);

CREATE OR REPLACE FUNCTION notify_new_message()
RETURNS TRIGGER AS $$
BEGIN
    -- Reserve senders only. Recipients may be names nobody has claimed yet,
    -- and reserving them would let any message block a future enrollment.
    INSERT INTO reserved_agent_names (name)
    SELECT NEW.sender
    WHERE NEW.sender ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'
    ON CONFLICT DO NOTHING;
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
`

const migrations = `
ALTER TABLE messages ADD COLUMN IF NOT EXISTS session_id TEXT;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_to INT;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'sent';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS session_id TEXT;
CREATE INDEX IF NOT EXISTS idx_messages_reply_to ON messages (reply_to);
UPDATE agents SET session_id = gen_random_uuid()::text WHERE session_id IS NULL;
ALTER TABLE agents ALTER COLUMN session_id SET NOT NULL;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'online';
UPDATE agents SET status = 'online' WHERE status IS NULL;
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash TEXT PRIMARY KEY,
    agent TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_agent ON api_keys (agent);
ALTER TABLE agents ADD COLUMN IF NOT EXISTS groups TEXT[] DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS memory_lead BOOLEAN NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS murmur_schema_migrations (name TEXT PRIMARY KEY);
DO $$
BEGIN
    INSERT INTO murmur_schema_migrations VALUES ('reserve-message-identities')
    ON CONFLICT DO NOTHING;
    IF FOUND THEN
        INSERT INTO reserved_agent_names (name)
        SELECT identity.name FROM (
            SELECT sender AS name FROM messages
            UNION
            SELECT substring(message FROM '^([a-zA-Z0-9][a-zA-Z0-9._-]{0,127}) (?:left$|joined\M)') FROM messages WHERE sender = 'system'
        ) identity
        WHERE identity.name ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'
        ON CONFLICT DO NOTHING;
    END IF;
END;
$$;
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages ("to");
`

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, migrations)
	return err
}

// ReserveAgentNames persists authorship so later purges cannot free an old name.
func ReserveAgentNames(ctx context.Context, pool *pgxpool.Pool, names []string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx, `INSERT INTO reserved_agent_names (name)
		SELECT name FROM unnest($1::text[]) AS authors(name)
		WHERE name ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'
		ON CONFLICT DO NOTHING`, names)
	return err
}
