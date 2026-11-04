-- +goose Up
-- External sign-in (OIDC, LDAP): a stable link from a user to the identity
-- that created it, and role bindings granted by group mapping.

-- Issuer of external_id: the OIDC issuer URL. (source, external_issuer,
-- external_id) identifies an external user even after the provider renames it.
ALTER TABLE users ADD COLUMN external_issuer text;
CREATE UNIQUE INDEX users_external_identity_idx ON users (source, external_issuer, external_id)
    WHERE external_id IS NOT NULL;

-- Source whose group mapping granted the binding; NULL for bindings an
-- administrator made. Mapped bindings are re-synced on every external
-- sign-in, manual ones are never touched by it.
ALTER TABLE role_bindings ADD COLUMN mapped_from text
    CHECK (mapped_from IN ('oidc', 'ldap', 'saml'));
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_mapped_user_chk
    CHECK (mapped_from IS NULL OR subject_kind = 'user');

-- +goose Down
DELETE FROM role_bindings WHERE mapped_from IS NOT NULL;
ALTER TABLE role_bindings DROP CONSTRAINT role_bindings_mapped_user_chk;
ALTER TABLE role_bindings DROP COLUMN mapped_from;
DROP INDEX users_external_identity_idx;
ALTER TABLE users DROP COLUMN external_issuer;
