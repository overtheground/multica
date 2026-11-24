ALTER TABLE agent
    DROP COLUMN IF EXISTS context_cap_tokens,
    DROP COLUMN IF EXISTS fresh_session_per_run;
