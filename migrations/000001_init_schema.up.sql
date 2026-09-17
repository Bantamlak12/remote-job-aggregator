-- Phase 1 schema: companies, target_companies, jobs.
--
-- job_eligibility is intentionally not created here: nothing populates it
-- yet, and it belongs to the filtering phase's migration instead.

-- Only bumps updated_at when some other column actually changed, so a
-- no-op UPDATE doesn't disturb it, and it still fires on a genuine change
-- even if that change also touched updated_at itself. search_path is
-- pinned per Postgres's own hardening guidance for SECURITY-sensitive
-- functions (trigger functions run with the privileges of the table
-- owner, not the caller).
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    IF to_jsonb(OLD) - 'updated_at' IS DISTINCT FROM to_jsonb(NEW) - 'updated_at' THEN
        NEW.updated_at = now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp;

-- Folds case and strips a trailing slash so 'https://acme.com' and
-- 'https://acme.com/' collide, and so does a different capitalization of
-- the same host. It does NOT normalize http vs https or www vs bare host
-- — those need real URL parsing, which SQL can't do safely, so a company
-- registered once as http://acme.com and again as https://acme.com will
-- still pass this constraint. Immutable because it's used in an index.
CREATE OR REPLACE FUNCTION normalize_website(url TEXT)
RETURNS TEXT AS $$
    SELECT lower(regexp_replace(btrim(url), '/+$', ''));
$$ LANGUAGE sql IMMUTABLE
SET search_path = pg_catalog, pg_temp;

CREATE TABLE companies (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL CHECK (length(btrim(name)) > 0),
    website     TEXT CHECK (website IS NULL OR length(btrim(website)) > 0),
    description TEXT,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Company identity is case-insensitive on name; website, when present, must
-- also be unique (after normalization) so the same employer can't be
-- registered twice under a slightly different name.
CREATE UNIQUE INDEX companies_name_lower_idx ON companies (lower(btrim(name)));
CREATE UNIQUE INDEX companies_website_idx ON companies (normalize_website(website)) WHERE website IS NOT NULL;

CREATE TRIGGER companies_set_updated_at
    BEFORE UPDATE ON companies
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TABLE target_companies (
    id                          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id                  BIGINT NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    -- ats_provider is intentionally not a CHECK-constrained enum: new
    -- providers are meant to be added without a schema migration. Valid
    -- values are enforced by the Go-side ats.Provider type instead.
    ats_provider                TEXT NOT NULL CHECK (length(btrim(ats_provider)) > 0),
    external_board_id           TEXT NOT NULL CHECK (length(btrim(external_board_id)) > 0),
    board_url                   TEXT,
    discovery_metadata          JSONB NOT NULL DEFAULT '{}',
    is_active                   BOOLEAN NOT NULL DEFAULT true,
    last_successful_ingestion_at TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Not used as a lookup key on their own; these exist purely so jobs'
    -- composite foreign keys below can force company_id and source to
    -- agree with the target row they claim to belong to.
    CONSTRAINT target_companies_id_company_id_key UNIQUE (id, company_id),
    CONSTRAINT target_companies_id_provider_key UNIQUE (id, ats_provider)
);

-- A target's real identity: the same provider+board can't be registered
-- twice, and this is what ingestion looks up by. Normalized on
-- external_board_id (trim + case-fold) for the same reason companies.name
-- and companies.website are: the CHECK constraint above only rejects
-- blank-after-trim, not a whitespace/case variant of an existing board id.
CREATE UNIQUE INDEX target_companies_provider_board_idx
    ON target_companies (ats_provider, lower(btrim(external_board_id)));

CREATE INDEX target_companies_company_id_idx ON target_companies (company_id);
CREATE INDEX target_companies_is_active_idx ON target_companies (is_active) WHERE is_active = true;

CREATE TRIGGER target_companies_set_updated_at
    BEFORE UPDATE ON target_companies
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

CREATE TABLE jobs (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id         BIGINT NOT NULL,
    target_company_id  BIGINT NOT NULL,

    -- Job identity per CLAUDE.md §9: source + source_job_id is primary;
    -- canonical_url is the fallback when a provider's ID is not stable.
    source             TEXT NOT NULL CHECK (length(btrim(source)) > 0),
    source_job_id      TEXT NOT NULL CHECK (length(btrim(source_job_id)) > 0),
    canonical_url      TEXT CHECK (canonical_url IS NULL OR length(btrim(canonical_url)) > 0),

    title              TEXT NOT NULL CHECK (length(btrim(title)) > 0),
    description        TEXT,
    application_url    TEXT NOT NULL CHECK (length(btrim(application_url)) > 0),
    location_raw       TEXT,

    remote_type        TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (remote_type IN ('remote', 'hybrid', 'onsite', 'unknown')),
    employment_type     TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (employment_type IN ('full_time', 'part_time', 'contract', 'internship', 'unknown')),

    published_at       TIMESTAMPTZ,
    first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_changed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at          TIMESTAMPTZ,

    status             TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed', 'removed')),
    content_hash       TEXT NOT NULL CHECK (length(btrim(content_hash)) > 0),

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT jobs_source_source_job_id_key UNIQUE (source, source_job_id),

    -- last_seen_at can never be earlier than first_seen_at; a job is
    -- "closed"/"removed" iff it has a closed_at.
    CONSTRAINT jobs_last_seen_after_first_seen CHECK (last_seen_at >= first_seen_at),
    CONSTRAINT jobs_closed_at_matches_status CHECK ((status IN ('closed', 'removed')) = (closed_at IS NOT NULL)),

    -- Forces company_id and source to actually match the target row this
    -- job claims to belong to — a job cannot be attributed to a company
    -- that doesn't own its board, and its source cannot drift from the
    -- board's own ats_provider (both would otherwise silently corrupt the
    -- (source, source_job_id) identity above).
    CONSTRAINT jobs_target_company_fk
        FOREIGN KEY (target_company_id, company_id) REFERENCES target_companies (id, company_id) ON DELETE CASCADE,
    CONSTRAINT jobs_target_source_fk
        FOREIGN KEY (target_company_id, source) REFERENCES target_companies (id, ats_provider) ON DELETE CASCADE
);

CREATE UNIQUE INDEX jobs_canonical_url_idx ON jobs (canonical_url) WHERE canonical_url IS NOT NULL;
CREATE INDEX jobs_company_id_idx ON jobs (company_id);
CREATE INDEX jobs_target_company_id_idx ON jobs (target_company_id);
-- Partial: "open" is the dominant query pattern (active listings); a full
-- index on a 3-value column would mostly be ignored by the planner anyway.
CREATE INDEX jobs_status_open_idx ON jobs (status) WHERE status = 'open';
CREATE INDEX jobs_last_seen_at_idx ON jobs (last_seen_at);

CREATE TRIGGER jobs_set_updated_at
    BEFORE UPDATE ON jobs
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();
