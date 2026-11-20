-- +goose Up
CREATE TABLE oidc_identity (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    user_id TEXT NOT NULL UNIQUE REFERENCES user(id) ON DELETE CASCADE,
    PRIMARY KEY (issuer, subject)
);

-- +goose Down
DROP TABLE IF EXISTS oidc_identity;
