-- users — the local mirror of the Appskep identity.
--
-- Nothing here authenticates anyone. Identity comes from the Appskep JWT; these
-- queries only keep the local mirror in step with it.

-- name: GetUser :one
SELECT * FROM users WHERE id = ? LIMIT 1;

-- name: GetUserByAppskepID :one
SELECT * FROM users WHERE appskep_user_id = ? LIMIT 1;

-- name: UpsertUserFromSSO :execresult
-- Called on every login. Refreshes the Appskep-owned fields and stamps the
-- login. It deliberately does NOT touch `role`: an admin demoted in the Phase 10
-- user panel must stay demoted through their next login. Bootstrap promotion
-- from ADMIN_USER_IDS is a separate SetUserRole call.
INSERT INTO users (appskep_user_id, email, name, is_active, last_login_at)
VALUES (?, ?, ?, 1, NOW())
ON DUPLICATE KEY UPDATE
  email         = VALUES(email),
  name          = VALUES(name),
  last_login_at = NOW();

-- name: SetUserRole :exec
UPDATE users SET role = ? WHERE id = ?;

-- name: SetUserActive :exec
UPDATE users SET is_active = ? WHERE id = ?;

-- name: UpdateUserProfile :exec
-- Only the locally-owned fields. name and email belong to Appskep and are
-- refreshed from the JWT on each login.
UPDATE users SET phone = ?, address = ? WHERE id = ?;

-- name: UpdateUserAvatar :exec
UPDATE users SET avatar_path = ? WHERE id = ?;

-- name: ListUsers :many
-- Pass "%" for an empty search box: name and email are NOT NULL, so LIKE '%'
-- matches every row and no SQL branching is needed.
SELECT * FROM users
WHERE name LIKE sqlc.arg(search) OR email LIKE sqlc.arg(search)
ORDER BY id DESC
LIMIT ? OFFSET ?;

-- name: CountUsers :one
SELECT COUNT(*) AS total FROM users WHERE name LIKE sqlc.arg(search) OR email LIKE sqlc.arg(search);

-- name: CountActiveAdmins :one
-- The last-admin guard: demoting or deactivating the only remaining admin locks
-- everyone out of the panel, and TPJ owns no credentials to get back in with.
SELECT COUNT(*) AS total FROM users WHERE role = 'admin' AND is_active = 1;

-- name: CountBookingsForUser :one
-- Shown on the user list, so an operator can see whether a row is a real
-- customer before deactivating it.
SELECT COUNT(*) AS total FROM bookings WHERE user_id = ?;
