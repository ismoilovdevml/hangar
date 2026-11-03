# Architecture decision records

Each record states the context, the decision and its consequences. Records are
immutable once accepted; a later record supersedes an earlier one explicitly.
Changing a decided part of the stack requires a new record.

| # | Title | Status |
|---|---|---|
| [0001](0001-go-chi-single-binary.md) | Go and chi, one `hangar` binary | Accepted |
| [0002](0002-postgresql-pgx-goose.md) | PostgreSQL with pgx and goose migrations | Accepted |
| [0003](0003-content-addressed-blobs.md) | Content-addressed, deduplicated blob storage | Accepted |
| [0004](0004-nats-jetstream.md) | NATS JetStream for events and jobs, embedded or clustered | Accepted |
| [0005](0005-react-spa-embedded.md) | React SPA embedded in the binary | Accepted |
| [0006](0006-clean-room.md) | Clean-room implementation | Accepted |
| [0007](0007-format-contract-and-routing.md) | Format contract and request routing | Accepted |
| [0008](0008-outbound-http-and-reverse-proxies.md) | Outbound HTTP proxies and running behind reverse proxies | Accepted |
| [0009](0009-rest-api-conventions.md) | REST API conventions | Accepted |

Template: copy the newest record, bump the number, keep it short.
