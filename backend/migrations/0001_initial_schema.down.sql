-- Reverse of 0001_initial_schema.up.sql.
--
-- Dropped in reverse dependency order. CASCADE is deliberately NOT used: if a
-- table has an unexpected dependent, this should fail loudly rather than silently
-- destroying something the migration did not create.

DROP TRIGGER IF EXISTS trg_agent_status_updated_at ON agent_status;
DROP TRIGGER IF EXISTS trg_user_settings_updated_at ON user_settings;
DROP TRIGGER IF EXISTS trg_notifications_updated_at ON notifications;
DROP TRIGGER IF EXISTS trg_prompt_queue_updated_at ON prompt_queue;
DROP TRIGGER IF EXISTS trg_sessions_updated_at ON sessions;
DROP TRIGGER IF EXISTS trg_repositories_updated_at ON repositories;
DROP TRIGGER IF EXISTS trg_devices_updated_at ON devices;
DROP TRIGGER IF EXISTS trg_oauth_accounts_updated_at ON oauth_accounts;
DROP TRIGGER IF EXISTS trg_users_updated_at ON users;

DROP FUNCTION IF EXISTS set_updated_at();

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS user_settings;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS agent_status;
DROP TABLE IF EXISTS prompt_queue;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS session_logs;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS oauth_accounts;
DROP TABLE IF EXISTS users;
