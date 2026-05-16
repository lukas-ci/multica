ALTER TABLE knowledge_sources ADD COLUMN sync_run_id             UUID;
ALTER TABLE knowledge_sources ADD COLUMN sync_started_at         TIMESTAMPTZ;
ALTER TABLE knowledge_sources ADD COLUMN sync_heartbeat_at       TIMESTAMPTZ;
ALTER TABLE knowledge_sources ADD COLUMN active_index_generation INT NOT NULL DEFAULT 0;
ALTER TABLE knowledge_sources ADD COLUMN sync_index_generation   INT;
ALTER TABLE knowledge_sources ADD COLUMN sync_watermark_at       TIMESTAMPTZ;
ALTER TABLE knowledge_sources ADD COLUMN legacy_index_mode       BOOLEAN NOT NULL DEFAULT true;
