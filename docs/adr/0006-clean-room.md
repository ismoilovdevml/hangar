# 0006. Clean-room implementation

Date: 2026-11-03 · Status: Accepted

## Context
Hangar addresses the same problem as commercial repository managers. Copying or
paraphrasing proprietary code would taint the project and its users.

## Decision
- Protocol behaviour is implemented only from public specifications (linked in
  `docs/FORMATS.md`) and from observing real clients (`docker`, `npm`, `mvn`,
  `pip`, `go`, `helm`, `cargo`, `dotnet`, `apt`) against Hangar.
- No proprietary product binaries, images or source are downloaded,
  decompiled, read or paraphrased.
- Open-source registries (e.g. distribution/distribution, Zot, Harbor,
  Verdaccio, Reposilite, Pulp) may be read for understanding; anything reused
  respects their licence and is attributed in the file that reuses it.
- Every format is accepted only when its real-client acceptance test passes.

## Consequences
- Compatibility is defined by what real clients accept, verified by e2e tests.
- Reviews reject changes whose origin cannot be explained from a public spec,
  client behaviour or a properly licensed open-source source.
