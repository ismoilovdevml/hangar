# FAQ

Short answers to common questions about Hangar: where it comes from, moving to it, offline use, limits, the database and security reports.

## Is Hangar a fork of another registry?

No. It is written from scratch in Go, from the public protocol specifications and the behaviour of real clients, and does not use code from other registry products.

## Can I move from another registry?

Yes. The `repository_import` task copies your published packages from any registry that speaks the format's protocol into hosted repositories, and `proxy_prewarm` fills the proxy caches from lockfiles or from the old registry. See [Move to Hangar](/docs/guides/migrate/).

## Does it work without internet?

Yes. Hosted repositories need nothing outside, and a proxy with `offline: true` serves only what it has cached. The install script installs from an internal mirror (`--mirror`) or a copied archive (`--files-from`), and scanning can use mirrored advisory databases.

## Are there user or storage limits?

Hangar has no user or seat limits and no activation key. Storage is limited by your disk or bucket.

## Which database?

PostgreSQL 16 or newer. Hangar applies its own schema migrations. Other databases are not supported.

## What is not done yet?

SAML sign-in, licence policies, SBOM download and edge replication are planned. The [roadmap](/docs/roadmap/) lists what is done, partial and planned.

## How do I report a security issue?

Privately, with **Report a vulnerability** on the [repository's Security tab](https://github.com/ismoilovdevml/hangar/security), not in a public issue.
