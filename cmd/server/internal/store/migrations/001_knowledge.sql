CREATE TABLE collections (
 name text PRIMARY KEY,
 description text NOT NULL DEFAULT '',
 visibility_groups text[] NOT NULL DEFAULT '{}'
);
CREATE TABLE documents (
 id text PRIMARY KEY,
 collection text NOT NULL REFERENCES collections(name) ON DELETE CASCADE,
 external_id text,
 title text NOT NULL,
 url text NOT NULL DEFAULT '',
 content text NOT NULL,
 metadata jsonb NOT NULL DEFAULT '{}',
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(collection, external_id)
);
CREATE TABLE chunks (
 document_id text NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
 chunk_index int NOT NULL,
 text text NOT NULL,
 search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED,
 embedding vector NOT NULL,
 embedding_dim int NOT NULL,
 embedding_provider text NOT NULL,
 PRIMARY KEY(document_id, chunk_index),
 CHECK (embedding_dim = vector_dims(embedding))
);
CREATE INDEX chunks_lexical ON chunks USING gin(search_vector);
CREATE TABLE sources (
 id text PRIMARY KEY,
 type text NOT NULL CHECK (type = 'notion'),
 collection text NOT NULL REFERENCES collections(name) ON DELETE CASCADE,
 config jsonb NOT NULL,
 last_sync jsonb
);
CREATE TABLE source_revisions (
 source_id text NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
 external_id text NOT NULL,
 edited text NOT NULL,
 document_id text NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
 PRIMARY KEY (source_id, external_id)
);
