-- name: ListExperiments :many
SELECT name, variants, config, url, is_active, created_at
FROM experiments
ORDER BY created_at DESC, name;

-- name: GetExperiment :one
SELECT name, variants, config, url, is_active, created_at
FROM experiments
WHERE name = $1;

-- name: GetExperimentForUpdate :one
SELECT name, variants, config, url, is_active, created_at
FROM experiments
WHERE name = $1
FOR UPDATE;

-- name: CreateExperiment :exec
INSERT INTO experiments (name, variants, config, url, is_active)
VALUES ($1, $2, $3, $4, $5);

-- name: UpdateExperiment :one
UPDATE experiments
SET variants = $2, config = $3, url = $4, is_active = $5
WHERE name = $1
RETURNING name, variants, config, url, is_active, created_at;

-- name: DeleteExperiment :execrows
DELETE FROM experiments
WHERE name = $1;
