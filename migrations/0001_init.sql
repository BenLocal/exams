-- Core schema for the exam aggregator.
--
-- Safe to run more than once: every statement is idempotent. If you apply this
-- by hand with psql, `exams migrate` still works afterwards.
--
-- All timestamps are timestamptz. Never use naive `timestamp` here: exam
-- announcements are published in Asia/Shanghai and rendering them in UTC
-- shifts dates by 8 hours.

CREATE TABLE IF NOT EXISTS sources (
    key            text PRIMARY KEY,
    name           text NOT NULL,
    base_url       text NOT NULL DEFAULT '',
    enabled        boolean NOT NULL DEFAULT true,
    -- Per-source cron spec. Empty means "use the global CRAWL_CRON".
    interval       text NOT NULL DEFAULT '',
    -- Set once the source has completed a successful run. The first crawl of a
    -- new source imports its whole backlog, which must not fire hundreds of
    -- notifications.
    bootstrap_done boolean NOT NULL DEFAULT false,
    last_run_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS exams (
    id            bigserial PRIMARY KEY,
    source_key    text NOT NULL REFERENCES sources (key) ON DELETE CASCADE,
    -- Stable identity within the source: the site's own id when it has one,
    -- otherwise the canonicalised detail URL.
    external_id   text NOT NULL,
    title         text NOT NULL,
    url           text NOT NULL DEFAULT '',
    summary       text NOT NULL DEFAULT '',
    -- Plain text, never HTML. Scraped markup must not reach the template layer.
    content       text NOT NULL DEFAULT '',
    category      text NOT NULL DEFAULT '',
    region        text NOT NULL DEFAULT '',
    published_at  timestamptz,
    -- The date exactly as the source printed it, e.g. "2026年9月23日". Kept so
    -- an ambiguous parse never destroys the original.
    published_raw text NOT NULL DEFAULT '',
    deadline_at   timestamptz,
    -- hash of the fields a list page exposes; drives whether the detail page
    -- needs fetching at all
    list_hash     text NOT NULL DEFAULT '',
    -- hash of the body text alone, so a content change can be detected without
    -- loading every body into memory
    body_hash     text NOT NULL DEFAULT '',
    is_read       boolean NOT NULL DEFAULT false,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT exams_source_external_uniq UNIQUE (source_key, external_id)
);

-- Primary list ordering. The id tiebreak is not optional: government sites
-- publish in batches on the same date, and LIMIT/OFFSET without a total order
-- shows duplicates across pages.
CREATE INDEX IF NOT EXISTS exams_published_idx
    ON exams (published_at DESC NULLS LAST, id DESC);
CREATE INDEX IF NOT EXISTS exams_read_published_idx
    ON exams (is_read, published_at DESC NULLS LAST, id DESC);
CREATE INDEX IF NOT EXISTS exams_source_idx ON exams (source_key);
CREATE INDEX IF NOT EXISTS exams_category_idx ON exams (category) WHERE category <> '';
CREATE INDEX IF NOT EXISTS exams_region_idx ON exams (region) WHERE region <> '';

CREATE TABLE IF NOT EXISTS crawl_runs (
    id          bigserial PRIMARY KEY,
    source_key  text NOT NULL,
    -- 'empty' is distinct from 'success' on purpose: a 200 response that
    -- parses to zero items is the signature of a broken selector or a WAF
    -- interstitial, and must not look healthy.
    status      text NOT NULL CHECK (status IN ('running', 'success', 'empty', 'failed')),
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    fetched     integer NOT NULL DEFAULT 0,
    inserted    integer NOT NULL DEFAULT 0,
    updated     integer NOT NULL DEFAULT 0,
    unchanged   integer NOT NULL DEFAULT 0,
    error       text NOT NULL DEFAULT '',
    note        text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS crawl_runs_source_idx ON crawl_runs (source_key, started_at DESC);
CREATE INDEX IF NOT EXISTS crawl_runs_started_idx ON crawl_runs (started_at DESC);

CREATE TABLE IF NOT EXISTS exam_changes (
    id             bigserial PRIMARY KEY,
    exam_id        bigint NOT NULL REFERENCES exams (id) ON DELETE CASCADE,
    run_id         bigint REFERENCES crawl_runs (id) ON DELETE SET NULL,
    change_type    text NOT NULL CHECK (change_type IN ('created', 'updated')),
    changed_fields text[] NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS exam_changes_exam_idx ON exam_changes (exam_id, created_at DESC);

-- Delivery ledger. Doubles as the dedupe key: a row is only re-attempted while
-- its status is not 'sent', so a transient SMTP failure does not permanently
-- suppress a notification.
CREATE TABLE IF NOT EXISTS notifications (
    id         bigserial PRIMARY KEY,
    exam_id    bigint NOT NULL REFERENCES exams (id) ON DELETE CASCADE,
    channel    text NOT NULL,
    target     text NOT NULL,
    event      text NOT NULL DEFAULT 'created',
    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts   integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,
    CONSTRAINT notifications_uniq UNIQUE (exam_id, channel, target)
);

CREATE INDEX IF NOT EXISTS notifications_created_idx ON notifications (created_at DESC);
CREATE INDEX IF NOT EXISTS notifications_retry_idx ON notifications (status)
    WHERE status <> 'sent';
