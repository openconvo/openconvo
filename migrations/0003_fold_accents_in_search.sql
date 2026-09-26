-- Fold accents in keyword search, so that café, cafe and CAFÉ match. Word
-- forms stay distinct: stemming depends on the language. unaccent ships with
-- PostgreSQL's contrib modules and is a trusted extension.
CREATE EXTENSION IF NOT EXISTS unaccent;

-- pg_restore --clean of a dump taken before this migration leaves the
-- configuration behind.
DROP TEXT SEARCH CONFIGURATION IF EXISTS openconvo_search;
CREATE TEXT SEARCH CONFIGURATION openconvo_search (COPY = simple);
ALTER TEXT SEARCH CONFIGURATION openconvo_search
    ALTER MAPPING FOR word, numword, hword, numhword, hword_part, hword_numpart
    WITH unaccent, simple;

-- Before PostgreSQL 17 a generated column cannot change its expression in
-- place, so this rewrites the messages table once (docs/upgrades.md).
ALTER TABLE messages DROP COLUMN search_vector;
ALTER TABLE messages ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (to_tsvector('openconvo_search', coalesce(content, ''))) STORED;
CREATE INDEX messages_search_idx ON messages USING gin (search_vector);
