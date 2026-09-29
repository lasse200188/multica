-- Custom (fork lasse200188/multica, see CUSTOM.md): personal Telegram
-- notifications for mentions and assignments. Additive only. No foreign keys
-- (repository rule); rows of a deleted user are inert because delivery starts
-- from an inbox item addressed to an existing user.
CREATE TABLE IF NOT EXISTS custom_telegram_link (
    user_id            UUID PRIMARY KEY,
    chat_id            BIGINT NOT NULL,
    telegram_username  TEXT,
    notify_mentions    BOOLEAN NOT NULL DEFAULT true,
    notify_assignments BOOLEAN NOT NULL DEFAULT true,
    linked_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS custom_telegram_link_token (
    token_hash TEXT PRIMARY KEY,
    user_id    UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
