package sqlitestore

const schema = `
CREATE TABLE IF NOT EXISTS memories (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL,
    origin_scope TEXT NOT NULL DEFAULT '',
    origin_id INTEGER,
    key TEXT,
    kind TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'manual',
    body TEXT NOT NULL,
    tags TEXT NOT NULL DEFAULT '[]',
    author TEXT NOT NULL,
    session_id TEXT,
    provenance TEXT NOT NULL,
    promoted_by TEXT,
    supersedes INTEGER REFERENCES memories(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    superseded_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_memories_author_created ON memories(author, created_at);
CREATE INDEX IF NOT EXISTS idx_memories_created ON memories(created_at);
CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memories_supersedes ON memories(supersedes) WHERE supersedes IS NOT NULL;
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    body, content='memories', content_rowid='id', tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS memories_fts_insert AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS memories_fts_delete AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS memories_fts_update AFTER UPDATE OF body ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO memories_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TABLE IF NOT EXISTS memory_schema_migrations (name TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS memory_searches (
    author TEXT NOT NULL,
    session_id TEXT NOT NULL,
    search_day TEXT NOT NULL,
    count INTEGER NOT NULL,
    PRIMARY KEY (author, session_id, search_day)
);
CREATE INDEX IF NOT EXISTS idx_memory_searches_day ON memory_searches(search_day);
`
