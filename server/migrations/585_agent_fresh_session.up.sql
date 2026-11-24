-- Per-agent session policy (MUL-207).
-- fresh_session_per_run: every issue run starts a new provider session instead
-- of resuming the previous one (heartbeat agents whose context would otherwise
-- grow without bound).
-- context_cap_tokens: a resumed issue session whose last reported context
-- exceeds this many tokens is dropped and the run starts fresh. NULL = no cap.
ALTER TABLE agent
    ADD COLUMN fresh_session_per_run BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN context_cap_tokens BIGINT CHECK (context_cap_tokens IS NULL OR context_cap_tokens > 0);
