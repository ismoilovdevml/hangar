# 0010. Sealed secrets bound to their row and field (hgs2)

Date: 2026-11-21 · Status: Accepted

## Context
Stored credentials (proxy upstream auth and URL passwords, S3 keys of blob
stores, OIDC/LDAP secrets, outbound proxy passwords, repository signing keys,
import task source credentials) are sealed by `internal/secrets` with
envelope encryption (AES-256-GCM data key per value, wrapped by
`security.secret_key`). Up to v0.1.x the format `hgs1.` used one fixed
additional data for every ciphertext, so a sealed value opened in any row of
any table. v0.1.9 closed the API replay (a ciphertext submitted as "my
password" was opened and sent to the submitter's host) by refusing sealed
input, but a value copied by other means (database access without the key, a
restored backup) still opened wherever it was put.

## Decision
- New format `hgs2.<key id>.<wrapped data key>.<ciphertext>`: same envelope,
  but the ciphertext's GCM additional data is the value's encryption context
  `"hgs2" || be32(len) table || be32(len) row || be32(len) field`, and the
  wrapped data key's is `"hgs2." || key id`. The context is not stored in the
  value; sealing and opening code both derive it from where the value lives.
- The row key is the row's identity, which never changes while the value is
  stored and is known before the row is written: the id of `repositories`,
  `blob_stores` and `tasks` (allocated with `gen_random_uuid()` before the
  insert, so a row re-created under the same name gets a new id), the key of
  `settings`, and the primary-key name of `outbound_proxies` and
  `signing_keys`. Fields are JSON paths (`proxy.auth.password`,
  `inventory.source.token`) or column names (`password`, `private_key`).
- `hgs1` values still open (the context is required but not bound). Every
  write produces `hgs2`; a kept legacy value is re-sealed on save. On start,
  `hangar serve` upgrades every stored `hgs1` value (the existing secrets
  sweep, one transaction, idempotent). Key rotation (`Rewrap`) accepts both
  formats and writes `hgs2`.
- The plaintext guards of migrations 00050/00090/00285 accept `hgs1.` and
  `hgs2.` (migration 00330); plaintext stays refused. Sealed input is refused
  by the API in either format.
- `hangar admin rotate-secrets --downgrade` writes every value back as `hgs1`,
  the only form Hangar before v0.2.0 reads.

## Consequences
- A sealed value moved to another row, field or table fails to open there
  (`ErrMalformed`); the startup sweep and `rotate-secrets` report it.
- Context binding does not protect the non-secret columns: someone who can
  write the database can still point a row's own URL elsewhere. Moving a
  secret between rows is what it prevents.
- Legacy `hgs1` values planted after the upgrade still open until the next
  sweep upgrades them; refusing `hgs1` outright is left for a later release.
- After the first start of v0.2.0 the database is no longer readable by
  v0.1.x. A downgrade needs `rotate-secrets --downgrade` first; a v0.1.x
  server started on `hgs2` values takes them for plaintext and seals them
  again, so its credentials stop working until re-entered.
