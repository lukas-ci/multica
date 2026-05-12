CREATE TABLE knowledge_sources (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    source_type   TEXT NOT NULL CHECK (source_type IN ('confluence', 'github', 'slack', 'google_drive')),
    display_name  TEXT NOT NULL,
    config        JSONB NOT NULL DEFAULT '{}',
    sync_status   TEXT NOT NULL DEFAULT 'pending' CHECK (sync_status IN ('pending', 'syncing', 'ready', 'error')),
    sync_error    TEXT,
    last_synced_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_knowledge_sources_workspace ON knowledge_sources(workspace_id);
