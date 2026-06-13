-- Migration 000010: create embeddings table for vector similarity search.
-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- Referential integrity enforced in the service layer (validate-on-write + nullable JOIN).
-- Requires the pgvector extension — ensure the Postgres image includes it
-- (e.g. pgvector/pgvector:pg17). Plain postgres:17-alpine does NOT include pgvector.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE embeddings (
    id            uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type   text         NOT NULL CHECK (entity_type IN ('tender', 'vendor')),
    entity_id     uuid         NOT NULL,
    embedding     vector(1536) NOT NULL,
    model_version text         NOT NULL CHECK (char_length(model_version) <= 100),
    created_at    timestamptz  NOT NULL DEFAULT now()
);

-- Unique index for upsert on (entity_type, entity_id).
-- Ensures at most one embedding row per entity.
CREATE UNIQUE INDEX embeddings_entity_type_entity_id_uidx
    ON embeddings (entity_type, entity_id);

-- HNSW index for fast approximate cosine similarity search.
-- ef_construction=64, m=16 are pgvector defaults; tune per workload.
CREATE INDEX embeddings_hnsw_cosine_idx
    ON embeddings
    USING hnsw (embedding vector_cosine_ops);
