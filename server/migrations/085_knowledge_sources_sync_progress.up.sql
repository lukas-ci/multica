ALTER TABLE knowledge_sources ADD COLUMN checkpoint        TEXT DEFAULT '';
ALTER TABLE knowledge_sources ADD COLUMN pages_fetched      INT NOT NULL DEFAULT 0;
ALTER TABLE knowledge_sources ADD COLUMN total_pages         INT;
ALTER TABLE knowledge_sources ADD COLUMN resources_fetched   INT NOT NULL DEFAULT 0;
ALTER TABLE knowledge_sources ADD COLUMN sync_kind           TEXT NOT NULL DEFAULT 'full' CHECK (sync_kind IN ('full', 'incremental'));
