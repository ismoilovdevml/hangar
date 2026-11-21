# Verify a release

Every release is built by the repository's release workflow
(`.github/workflows/release.yml`) from a version tag, after the test suite and
the security scans pass. From v0.2.0 on, that workflow also signs what it
publishes, without a long-lived key: each signature carries a short-lived
[Sigstore](https://www.sigstore.dev/) certificate that names the workflow and
the tag it ran for, and is recorded in the public Rekor transparency log.
Earlier releases have `SHA256SUMS` only.

| Artifact | Signature | SBOM (SPDX JSON) | Build provenance (SLSA) |
|---|---|---|---|
| `ghcr.io/ismoilovdevml/hangar:<version>` | cosign, the index and each platform | cosign attestation per platform, and on the release page | GitHub attestation, also stored in the registry |
| `ghcr.io/ismoilovdevml/hangar-scanner:<version>` | cosign, the index and each platform | cosign attestation per platform, and on the release page | GitHub attestation, also stored in the registry |
| `oci://ghcr.io/ismoilovdevml/charts/hangar`, version `<version>` | cosign | | GitHub attestation, also stored in the registry |
| Release files (binaries, `install.sh`, `hangar-compose.tar.gz`, SBOMs, `SHA256SUMS`) | `<file>.sigstore.json` bundle next to each file | `hangar-linux-<arch>.spdx.json` for the binaries | GitHub attestation |

You need [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
for signatures and SBOMs, and the [GitHub CLI](https://cli.github.com/) (`gh`)
for build provenance. The commands below have not yet been run against a
published release: v0.2.0 is the first one signed this way. If a command
fails on a release that should be signed, report it as described in
[SECURITY.md](../../SECURITY.md).

## The identity to trust

A signature is only worth the identity you require. Release signatures come
from this workflow file, run for a version tag:

```bash
VERSION=0.2.0   # the release, without the "v"
ISSUER=https://token.actions.githubusercontent.com
IDENTITY="https://github.com/ismoilovdevml/hangar/.github/workflows/release.yml@refs/tags/v$VERSION"
```

Use `--certificate-identity "$IDENTITY"` to accept exactly that release. To
accept any release (for example in an admission policy), use this regular
expression instead. It also accepts a release re-run by hand from `main`
(the workflow's *Run workflow* button), which signs with
`@refs/heads/main`; drop that alternative if you only trust tag pushes:

```bash
IDENTITY_RE='^https://github\.com/ismoilovdevml/hangar/\.github/workflows/release\.yml@refs/(tags/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?|heads/main)$'
```

Every command below takes either
`--certificate-identity "$IDENTITY"` or
`--certificate-identity-regexp "$IDENTITY_RE"`, always together with
`--certificate-oidc-issuer "$ISSUER"`. Never verify without them: a
signature by anyone else's workflow would pass.

## Container images

```bash
cosign verify "ghcr.io/ismoilovdevml/hangar:$VERSION" \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"

cosign verify "ghcr.io/ismoilovdevml/hangar-scanner:$VERSION" \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
```

A tag can be moved; a digest cannot. Verify, then deploy the digest that was
verified (`cosign verify` prints it as `docker-manifest-digest`):

```bash
cosign verify "ghcr.io/ismoilovdevml/hangar:$VERSION" \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" |
  jq -r '.[0].critical.image."docker-manifest-digest"'
```

Each platform's manifest is signed too, so pulling
`ghcr.io/ismoilovdevml/hangar@sha256:<platform digest>` verifies the same way.

### SBOMs

The SPDX SBOM of each platform is attached to the image as a signed
attestation:

```bash
cosign verify-attestation --type spdxjson "ghcr.io/ismoilovdevml/hangar:$VERSION" \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" |
  jq -r '.payload' | base64 -d | jq -r '.predicate.name'
```

To feed an SBOM to a scanner, take the predicate of one of them
(`... | base64 -d | jq '.predicate' > hangar.spdx.json`), or download
`hangar-image-linux-amd64.spdx.json` (and `arm64`, and the
`hangar-scanner-image-*` files) from the release page and verify its bundle
like any other release file.

## Helm chart

```bash
cosign verify "ghcr.io/ismoilovdevml/charts/hangar:$VERSION" \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"

helm install hangar oci://ghcr.io/ismoilovdevml/charts/hangar --version "$VERSION"
```

The chart installs the two images above at the same version, so verify them
as well.

## Binaries, install.sh and the compose stack

Download the files you need with their `.sigstore.json` bundles and
`SHA256SUMS` from the release page, then check the sums and the signature of
`SHA256SUMS`, which covers every file:

```bash
base=https://github.com/ismoilovdevml/hangar/releases/download/v$VERSION
curl -fsSLO "$base/hangar-linux-amd64"
curl -fsSLO "$base/SHA256SUMS"
curl -fsSLO "$base/SHA256SUMS.sigstore.json"

cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
sha256sum -c SHA256SUMS --ignore-missing
```

Each file also has its own bundle, so one file can be checked without
`SHA256SUMS`. For the installer, verify before you run it instead of piping
it into a shell:

```bash
curl -fsSLO "$base/install.sh"
curl -fsSLO "$base/install.sh.sigstore.json"
cosign verify-blob install.sh --bundle install.sh.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
sudo bash install.sh --version "v$VERSION"
```

## Build provenance

The release workflow records SLSA build provenance for both images, the chart
and every release file: which repository, workflow, commit and runner built
it. Check it with the GitHub CLI:

```bash
gh attestation verify "oci://ghcr.io/ismoilovdevml/hangar:$VERSION" --repo ismoilovdevml/hangar
gh attestation verify "oci://ghcr.io/ismoilovdevml/hangar-scanner:$VERSION" --repo ismoilovdevml/hangar
gh attestation verify "oci://ghcr.io/ismoilovdevml/charts/hangar:$VERSION" --repo ismoilovdevml/hangar
gh attestation verify hangar-linux-amd64 --repo ismoilovdevml/hangar
```

## Enforcing it in Kubernetes

An admission controller can refuse unsigned images: Kyverno's
`verifyImages` rule or the Sigstore policy-controller, with the issuer above
and `IDENTITY_RE` as the subject regular expression, for
`ghcr.io/ismoilovdevml/hangar*`. Pin the server image by the verified digest
with the chart's `image.digest` value (it takes precedence over `image.tag`),
so what runs is what was checked. The scan adapter image
(`imageScanning.scanner.image`) has no digest value; it is pulled by tag.

## Mirrors and air-gapped installs

`cosign copy` copies an image together with its signatures and attestations:

```bash
cosign copy "ghcr.io/ismoilovdevml/hangar:$VERSION" "registry.internal.example/hangar:$VERSION"
```

Verify against the mirror with the same identity. Offline verification
(without reaching Rekor) needs the bundle and a trusted root: see
`cosign verify --help` (`--offline`, `--trusted-root`).
