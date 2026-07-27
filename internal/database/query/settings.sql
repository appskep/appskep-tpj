-- settings — key/value site configuration, editable from /admin/pengaturan.
--
-- Where a key also exists in the environment (payment_expiry_minutes vs
-- PAYMENT_EXPIRY_MINUTES), the DB row wins when present and parseable; the env
-- value is the fallback and the fail-fast default. config stays the only reader
-- of os.Getenv.

-- name: GetSetting :one
SELECT setting_value FROM settings WHERE setting_key = ? LIMIT 1;

-- name: ListSettings :many
SELECT * FROM settings ORDER BY setting_key ASC;

-- name: UpsertSetting :exec
INSERT INTO settings (setting_key, setting_value) VALUES (?, ?)
ON DUPLICATE KEY UPDATE setting_value = VALUES(setting_value);

-- name: DeleteSetting :exec
DELETE FROM settings WHERE setting_key = ?;
