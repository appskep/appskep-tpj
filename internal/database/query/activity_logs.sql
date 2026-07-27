-- activity_logs — audit trail for admin actions and booking state changes.

-- name: CreateActivityLog :exec
-- meta is a JSON column: pass NULL or valid JSON, never an empty string.
INSERT INTO activity_logs (user_id, action, entity, entity_id, meta, ip)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListActivityLogsForEntity :many
-- The booking detail timeline in the admin panel.
SELECT * FROM activity_logs
WHERE entity = ? AND entity_id = ?
ORDER BY id DESC
LIMIT ?;

-- name: ListActivityLogsByUser :many
SELECT * FROM activity_logs
WHERE user_id = ?
ORDER BY id DESC
LIMIT ? OFFSET ?;
