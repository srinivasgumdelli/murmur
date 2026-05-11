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
ALTER TABLE agents ADD COLUMN IF NOT EXISTS groups TEXT[] DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS description TEXT;
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages ("to");
`

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, migrations)
	return err
}
