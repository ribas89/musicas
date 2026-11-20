-- +goose Up
-- Profile image is linked to the verified (issuer,subject), not editable as a user field.
ALTER TABLE oidc_identity ADD COLUMN avatar TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE oidc_identity DROP COLUMN avatar;
