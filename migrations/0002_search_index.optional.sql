-- Trigram indexes for Chinese substring search.
--
-- This migration is OPTIONAL: it needs privileges to CREATE EXTENSION, which a
-- managed PostgreSQL may not grant you. Without it the app still returns
-- correct results — `ILIKE '%kw%'` just falls back to a sequential scan, which
-- is fine at the scale this tool operates on.
--
-- If `exams migrate` warns that this was skipped, either ask an admin to run
--   CREATE EXTENSION pg_trgm;
-- or run this file yourself as a superuser. `exams migrate` retries optional
-- migrations on every invocation, so granting the privilege is enough.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS exams_title_trgm_idx
    ON exams USING gin (title gin_trgm_ops);
CREATE INDEX IF NOT EXISTS exams_summary_trgm_idx
    ON exams USING gin (summary gin_trgm_ops);
