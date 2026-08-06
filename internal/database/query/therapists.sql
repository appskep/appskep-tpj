-- therapists — the terapis profiles, and which layanan each one handles.

-- name: GetTherapist :one
SELECT * FROM therapists WHERE id = ? LIMIT 1;

-- name: GetActiveTherapistBySlug :one
-- The public profile page. Inactive therapists must 404, not render.
SELECT * FROM therapists WHERE slug = ? AND is_active = 1 LIMIT 1;

-- name: ListActiveTherapists :many
-- Public listing.
SELECT * FROM therapists WHERE is_active = 1 ORDER BY sort_order ASC, id ASC;

-- name: HasActiveTherapists :one
-- Whether the public site advertises the terapis section at all. EXISTS rather
-- than a COUNT: the answer is a yes/no, and idx_therapists_active_sort makes it
-- an index-only lookup.
SELECT EXISTS(SELECT 1 FROM therapists WHERE is_active = 1) AS present;

-- name: ListTherapistsAdmin :many
-- Pass "%" for an empty search box.
SELECT * FROM therapists
WHERE name LIKE sqlc.arg(search)
ORDER BY sort_order ASC, id ASC
LIMIT ? OFFSET ?;

-- name: CountTherapistsAdmin :one
SELECT COUNT(*) AS total FROM therapists WHERE name LIKE sqlc.arg(search);

-- name: TherapistSlugTaken :one
-- Pass 0 as the id when creating, so no row is excluded.
SELECT EXISTS(SELECT 1 FROM therapists WHERE slug = ? AND id <> ?) AS taken;

-- name: CreateTherapist :execresult
INSERT INTO therapists (slug, name, specialization, bio, certifications,
                        years_experience, image_path, is_active, sort_order)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateTherapist :exec
-- image_path is updated separately so a form submit without a new file cannot
-- blank an existing photo. slug is absent for the same reason it is absent from
-- UpdateService: it is the public URL and is allocated once, at creation.
UPDATE therapists SET
  name             = ?,
  specialization   = ?,
  bio              = ?,
  certifications   = ?,
  years_experience = ?,
  is_active        = ?,
  sort_order       = ?
WHERE id = ?;

-- name: UpdateTherapistImage :exec
UPDATE therapists SET image_path = ? WHERE id = ?;

-- name: SetTherapistActive :exec
UPDATE therapists SET is_active = ? WHERE id = ?;

-- name: DeleteTherapist :execresult
-- Nothing references a therapist except therapist_services, which CASCADEs, so
-- unlike DeleteService this can never be blocked by a foreign key.
DELETE FROM therapists WHERE id = ?;

-- ---------------------------------------------------------------------------
-- therapist_services
-- ---------------------------------------------------------------------------

-- name: ListServicesForTherapist :many
-- The public profile page. Active layanan only, in the catalogue's own order.
SELECT s.* FROM services s
JOIN therapist_services ts ON ts.service_id = s.id
WHERE ts.therapist_id = ? AND s.is_active = 1
ORDER BY s.sort_order ASC, s.id ASC;

-- name: ListServiceIDsForTherapist :many
-- Drives the admin form's checkbox state. Deliberately unfiltered: a tag
-- pointing at a deactivated layanan must still be visible to the service layer,
-- which decides what to do with it.
SELECT service_id FROM therapist_services WHERE therapist_id = ?;

-- name: ListServiceTagsForActiveTherapists :many
-- One query for the whole /terapis grid, grouped by therapist_id in Go. The
-- alternative is a ListServicesForTherapist per card.
SELECT ts.therapist_id, s.id, s.slug, s.name
FROM therapist_services ts
JOIN services s   ON s.id = ts.service_id
JOIN therapists t ON t.id = ts.therapist_id
WHERE t.is_active = 1 AND s.is_active = 1
ORDER BY ts.therapist_id ASC, s.sort_order ASC, s.id ASC;

-- name: ListActiveTherapistsForService :many
-- The reverse link, for the strip on /layanan/{slug}.
SELECT t.* FROM therapists t
JOIN therapist_services ts ON ts.therapist_id = t.id
WHERE ts.service_id = ? AND t.is_active = 1
ORDER BY t.sort_order ASC, t.id ASC;

-- name: DeleteTherapistServices :exec
DELETE FROM therapist_services WHERE therapist_id = ?;

-- name: AddTherapistService :exec
INSERT INTO therapist_services (therapist_id, service_id) VALUES (?, ?);
