-- Context size the model actually received per run (MUL-197): input + cache
-- read + cache creation tokens of each model call. One row per task.
CREATE TABLE task_context_usage (
    task_id UUID PRIMARY KEY REFERENCES agent_task_queue(id) ON DELETE CASCADE,
    peak_context_tokens BIGINT NOT NULL DEFAULT 0,
    sum_context_tokens BIGINT NOT NULL DEFAULT 0,
    call_count INTEGER NOT NULL DEFAULT 0,
    last_context_tokens BIGINT NOT NULL DEFAULT 0,
    last_cache_read_tokens BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_task_context_usage_updated_at ON task_context_usage(updated_at);
