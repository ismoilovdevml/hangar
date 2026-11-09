# APT (Debian, Ubuntu)

Hangar serves APT repositories in the Debian archive layout
(`dists/` and `pool/`). `apt-get update`, `apt-get install`, `apt download` and
unattended upgrades work against hosted, proxy and group repositories.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<user>` and `<token>`: your user name and a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

Repository URL: `https://hangar.example.com/repository/<repo>/`.

Hosted and group repositories sign their metadata with an OpenPGP key. Signing
keys are stored encrypted with `security.secret_key` (`HANGAR_SECRET_KEY`).
Without that key, InRelease and the public key answer `501`.

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "apt-hosted",
        "format": "apt",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow_once" } }
      }'
```

Format settings (`config.format`):

| Setting | Default | Meaning |
|---|---|---|
| `architectures` | `["amd64", "arm64"]` | Listed in every distribution besides the architectures of uploaded packages. Clients of a listed architecture get (possibly empty) indexes. |
| `signing_key` | `repository` | `repository`: the repository has its own key. `shared`: it signs with the server-wide shared key. |

In the web UI both settings are on the repository form (**Architectures**
and **Sign with**) for hosted and group repositories; proxies have neither.

Hangar stores each package once under
`pool/<component>/<letter or libX>/<source>/<package>_<version>_<arch>.deb`. After
every upload or delete it regenerates `dists/`:

- per distribution, component and architecture: `Packages`, `Packages.gz` and `Packages.xz`;
- per distribution: a `Release` file with `MD5Sum`, `SHA1`, `SHA256` and `SHA512` sections;
- `InRelease` (clearsigned) and `Release.gpg` (detached), signed when they are requested.

Packages with `Architecture: all` are listed under every architecture. A
`Release` whose content did not change keeps its `Date`.

### Proxy

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "debian-proxy",
        "format": "apt",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "http://deb.debian.org/debian", "metadata_ttl": "30m" } }
      }'
```

- `remote_url` is the archive root, the directory that holds `dists/` and
  `pool/`. Examples: `http://deb.debian.org/debian`,
  `http://deb.debian.org/debian-security`, `http://archive.ubuntu.com/ubuntu`.
  Plain `http` is fine: apt verifies the signatures, not the transport.
- `InRelease`, `Release` and `Release.gpg` are passed through byte for byte,
  so clients verify the remote's own signature with the distribution keyring.
  They are revalidated after `metadata_ttl` (`0` means 30 minutes). While the
  remote is down, the cached copy is served.
- `Packages` and other index files are served while they match the hashes of
  the cached `Release`. When they no longer match, they are fetched again.
- `by-hash/` files and `pool/` packages are cached forever. Distributions that
  set `Acquire-By-Hash: yes`, such as Debian and Ubuntu, are fetched by hash by
  apt.
- A proxy has no signing key of its own, so `public.key` answers `404`.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{ "name": "apt-all", "format": "apt", "type": "group",
        "config": { "group": { "members": ["apt-hosted", "apt-team"] } } }'
```

**APT groups combine hosted repositories only.** Their `Packages` and
`Release` files are regenerated from the members' packages and signed with
the group's own key. If two members hold the same package version and
architecture, the first member wins.

A group with a proxy member answers `400` with an explanation. A remote's
signed `Release` cannot be merged with other packages without either
breaking its signature or re-signing someone else's archive. Point clients at
the proxy directly, next to the group.

## Client setup

### Hosted or group repository

```bash
# The signing key, in the format Signed-By expects
curl -fsSL -u <user>:<token> https://hangar.example.com/repository/apt-hosted/public.gpg \
  | sudo tee /etc/apt/keyrings/apt-hosted.gpg > /dev/null

sudo tee /etc/apt/sources.list.d/apt-hosted.sources <<'EOF'
Types: deb
URIs: https://hangar.example.com/repository/apt-hosted/
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/apt-hosted.gpg
EOF

# Private repositories: apt reads credentials from auth.conf.d, never from the URL
echo 'machine hangar.example.com login <user> password <token>' \
  | sudo tee /etc/apt/auth.conf.d/hangar.conf > /dev/null
sudo chmod 600 /etc/apt/auth.conf.d/hangar.conf

sudo apt-get update
sudo apt-get install hello
```

- `public.key` is the same key, ASCII-armored, for `apt-key`-era tools.
- Older apt (Debian 10, Ubuntu 18.04) reads one-line `.list` files:
  `deb [signed-by=/etc/apt/keyrings/apt-hosted.gpg] https://hangar.example.com/repository/apt-hosted/ stable main`.
- Repositories with anonymous read need neither `-u` nor `auth.conf.d`.
- apt sends `auth.conf.d` credentials over plain `http` only when the entry
  names the scheme: `machine http://hangar.example.com:8080 login ...`.

### Proxy of Debian or Ubuntu

```bash
sudo tee /etc/apt/sources.list.d/debian.sources <<'EOF'
Types: deb
URIs: https://hangar.example.com/repository/debian-proxy/
Suites: bookworm bookworm-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF
```

On Ubuntu use `/usr/share/keyrings/ubuntu-archive-keyring.gpg`.

## Upload

```bash
# Raw body; distribution and component default to stable / main
curl -u <user>:<token> --upload-file ./hello_2.10-3_amd64.deb \
  "https://hangar.example.com/repository/apt-hosted/upload?distribution=bookworm&component=main"

# Multipart form
curl -u <user>:<token> -F distribution=bookworm -F component=main -F package=@hello_2.10-3_amd64.deb \
  https://hangar.example.com/repository/apt-hosted/upload
```

- The control file is read from the package (`control.tar.gz`, `.xz`, `.zst`
  or uncompressed). `Package`, `Version` and `Architecture` are required.
- The response is `201` with the pool path. Uploading the same file to
  another distribution lists it there too; it is still stored once.
- Uploading through a group needs `write_member`.

## Delete

```bash
curl -u <user>:<token> -X DELETE \
  https://hangar.example.com/repository/apt-hosted/pool/main/h/hello/hello_2.10-3_amd64.deb
```

Deleting a version in the web UI, or through cleanup policies, regenerates the
indexes too. Quarantined packages leave the indexes until they are released.

## Signing keys

The key is generated when the repository is created: RSA 4096, which every
apt and gpgv release in use accepts. Admin API:

```bash
# Details and the armored public key
curl -H "Authorization: Bearer $HANGAR_TOKEN" https://hangar.example.com/api/v1/repositories/apt-hosted/signing-key

# Rotate (body optional; "ed25519" needs apt with gpgv 2.1+)
curl -X POST -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"algorithm": "rsa4096"}' https://hangar.example.com/api/v1/repositories/apt-hosted/signing-key:rotate

# Import an existing key
jq -n --rawfile k private.asc --arg p "$PASSPHRASE" '{private_key: $k, passphrase: $p}' |
  curl -X PUT -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
    --data-binary @- https://hangar.example.com/api/v1/repositories/apt-hosted/signing-key
```

After a rotation or import, metadata is signed with the new key at once.
Clients fail with `NO_PUBKEY` until they download the new `public.gpg`.
Rotating needs repository administration; the shared key (`signing_key:
shared`) needs `system:repos:admin`. Both are audited (`signing_key.rotate`,
`signing_key.import`).

In the web UI, the **Signing key** panel on the settings page of a hosted or
group repository shows the fingerprint, algorithm and creation date, whether
the key is the repository's own or the shared one, and links to `public.gpg`
and `public.key`. **Rotate** asks for confirmation first; **Import key** takes
a pasted ASCII-armored private key and an optional passphrase, which are sent
once and never shown again. Users without the rights above see the buttons
disabled with the reason. Without `HANGAR_SECRET_KEY` the panel says so.

## Vulnerability scanning

Debian and Ubuntu advisories (OSV `Debian:12`, `Ubuntu:22.04:LTS`, ...) only
apply to one distribution release, so a repository is scanned once its
release is known:

- a proxy of a Debian or Ubuntu archive detects it from the suites clients
  fetch through it (`bookworm`, `bookworm-updates` and `bookworm-security`
  are `Debian:12`); one proxy serving two releases is not guessed;
- otherwise set **Distribution release** in the repository settings, or in
  the API `"config": {"scanning": {"distro": "Debian:12"}}`. The setting
  wins over the detection.

Packages are checked by their source package (`libssl3` as `openssl`) and
version, the fix plan suggests `apt-get install --only-upgrade <packages>`.
Until the release is known the packages show "No advisory source". Details:
[vulnerabilities.md](vulnerabilities.md#apt-and-rpm-distribution-releases).

## Limits

- Only binary packages (`.deb`) are indexed. Source packages and `Contents-*`
  files are not generated.
- Hosted `Release` files do not set `Acquire-By-Hash`. A client whose
  `apt-get update` runs at the same moment as an upload can see a hash mismatch;
  the next update succeeds.
- Packages themselves are not re-signed (Debian packages are rarely signed
  individually; the signed `Release` covers them through the hashes).
