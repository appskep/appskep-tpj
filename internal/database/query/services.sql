-- services — the layanan catalogue.

-- name: GetService :one
SELECT * FROM services WHERE id = ? LIMIT 1;

-- name: GetServiceBySlug :one
SELECT * FROM services WHERE slug = ? LIMIT 1;

-- name: GetActiveServiceBySlug :one
-- The public detail page. Inactive services must 404, not render.
SELECT * FROM services WHERE slug = ? AND is_active = 1 LIMIT 1;

-- name: ListActiveServices :many
-- Public listing. Includes coming-soon services: they are shown with a label.
SELECT * FROM services WHERE is_active = 1 ORDER BY sort_order ASC, id ASC;

-- name: ListBookableServices :many
-- Booking step 1. Coming-soon services appear on the public site but cannot be
-- booked, so they are excluded here.
SELECT * FROM services
WHERE is_active = 1 AND is_coming_soon = 0
ORDER BY sort_order ASC, id ASC;

-- name: ListServicesAdmin :many
-- Pass "%" for an empty search box.
SELECT * FROM services
WHERE name LIKE sqlc.arg(search)
ORDER BY sort_order ASC, id ASC
LIMIT ? OFFSET ?;

-- name: CountServicesAdmin :one
SELECT COUNT(*) AS total FROM services WHERE name LIKE sqlc.arg(search);

-- name: ServiceSlugTaken :one
-- Pass 0 as the id when creating, so no row is excluded.
SELECT EXISTS(SELECT 1 FROM services WHERE slug = ? AND id <> ?) AS taken;

-- name: CreateService :execresult
INSERT INTO services (slug, name, description, price, duration_minutes, image_path,
                      is_active, is_coming_soon, sort_order)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateService :exec
-- image_path is updated separately so a form submit without a new file cannot
-- blank an existing image.
UPDATE services SET
  slug             = ?,
  name             = ?,
  description      = ?,
  price            = ?,
  duration_minutes = ?,
  is_active        = ?,
  is_coming_soon   = ?,
  sort_order       = ?
WHERE id = ?;

-- name: UpdateServiceImage :exec
UPDATE services SET image_path = ? WHERE id = ?;

-- name: SetServiceActive :exec
UPDATE services SET is_active = ? WHERE id = ?;

-- name: SetServiceComingSoon :exec
UPDATE services SET is_coming_soon = ? WHERE id = ?;

-- name: CountBookingsForService :one
-- The admin panel calls this before offering delete.
SELECT COUNT(*) AS total FROM bookings WHERE service_id = ?;

-- name: DeleteService :execresult
-- Blocked by fk_bookings_service (RESTRICT) once any booking references it. The
-- handler checks CountBookingsForService first and offers deactivation instead.
DELETE FROM services WHERE id = ?;
