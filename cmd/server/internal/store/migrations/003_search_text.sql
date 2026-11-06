-- The lexical index is built from Go-tokenised text (NFKC, lower-case, CJK
-- bigrams) instead of Postgres' whitespace-only `simple` parser, so Japanese
-- prose matches single words such as "ceatec". Existing rows are
-- re-tokenised by the server on startup (store.ReindexSearchText).
DROP INDEX chunks_lexical;
ALTER TABLE chunks DROP COLUMN search_vector;
ALTER TABLE chunks ADD COLUMN search_text text NOT NULL DEFAULT '';
ALTER TABLE chunks ADD COLUMN search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', search_text)) STORED;
CREATE INDEX chunks_lexical ON chunks USING gin(search_vector);
