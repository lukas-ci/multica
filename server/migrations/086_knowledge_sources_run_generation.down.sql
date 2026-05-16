ALTER TABLE knowledge_sources DROP COLUMN legacy_index_mode;
ALTER TABLE knowledge_sources DROP COLUMN sync_watermark_at;
ALTER TABLE knowledge_sources DROP COLUMN sync_index_generation;
ALTER TABLE knowledge_sources DROP COLUMN active_index_generation;
ALTER TABLE knowledge_sources DROP COLUMN sync_heartbeat_at;
ALTER TABLE knowledge_sources DROP COLUMN sync_started_at;
ALTER TABLE knowledge_sources DROP COLUMN sync_run_id;
