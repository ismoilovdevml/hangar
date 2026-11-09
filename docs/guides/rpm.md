# RPM (RHEL, Rocky, AlmaLinux, Fedora)

Hangar serves YUM/DNF repositories in the createrepo layout: a
`repodata/repomd.xml` that points at `primary.xml.gz`, `filelists.xml.gz` and
`other.xml.gz`, next to the `.rpm` files. `dnf install`, `dnf makecache`,
`yum install` and `dnf download` work against hosted, proxy and group
repositories.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<user>` and `<token>`: your user name and a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

Repository URL, which is the `baseurl`: `https://hangar.example.com/repository/<repo>/`.

Hosted and group repositories sign `repomd.xml` with an OpenPGP key, for
`repo_gpgcheck=1`. Signing keys are stored encrypted with
`security.secret_key` (`HANGAR_SECRET_KEY`).

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "rpm-hosted",
        "format": "rpm",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow_once" } }
      }'
```

Format settings (`config.format`):

| Setting | Default | Meaning |
|---|---|---|
| `repodata_depth` | `0` | `0`: the whole repository is one yum repository. `N` (1 to 4): every directory `N` levels deep gets its own `repodata/`. With `2`, packages under `el9/x86_64/` and `el8/x86_64/` form two repositories, each with its own `baseurl`. |
| `signing_key` | `repository` | `repository`: the repository has its own key. `shared`: it signs with the server-wide shared key. |

In the web UI both settings are on the repository form (**Repodata depth**
and **Sign with**) for hosted and group repositories; proxies have neither.

For every package Hangar parses the RPM header in Go and keeps:

- the name, epoch, version, release and arch;
- the summary, description, licence and URL;
- the source RPM;
- requires, provides, conflicts and obsoletes, and the weak dependencies;
- the files and the changelog;
- the package sha256 and the header byte range.

After every upload or delete it regenerates `repodata/`:

- sha256 checksums, and file names prefixed with the content hash;
- a signature, `repomd.xml.asc`, made when it is requested. dnf reads
  `repomd.xml` and then `repomd.xml.asc`; when an upload replaces `repomd.xml`
  between the two, the same client (credentials, address and user agent) still
  gets the signature of the document it read, for two minutes;
- `repomd.xml.key`, the public key.

The superseded files stay available for an hour, for clients that fetched the
old `repomd.xml`. A repository without packages serves an empty repodata, so
clients configured before the first upload do not fail.

### Proxy

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "rocky9-baseos",
        "format": "rpm",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/", "metadata_ttl": "30m" } }
      }'
```

- `remote_url` is the repository root, the directory that contains `repodata/`.
  Examples:
  - `https://dl.rockylinux.org/pub/rocky/9/AppStream/x86_64/os/`
  - `https://dl.fedoraproject.org/pub/epel/9/Everything/x86_64/`
  - `https://repo.almalinux.org/almalinux/9/BaseOS/x86_64/os/`

  Mirror lists and metalinks are not followed: give one mirror's URL.
- `repomd.xml`, `repomd.xml.asc` and `repomd.xml.key` are passed through
  unchanged and revalidated after `metadata_ttl` (`0` means 30 minutes). While
  the remote is down, the cached copy is served.
- Other `repodata/` files are served while they match the checksums in the
  cached `repomd.xml`; their names are content-hashed. Packages are cached
  forever.
- Packages keep their distribution signature. Use `gpgcheck=1` with the
  distribution's key.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{ "name": "rpm-all", "format": "rpm", "type": "group",
        "config": { "group": { "members": ["rpm-hosted", "rpm-team"] } } }'
```

**RPM groups combine hosted repositories only.** Their repodata is
regenerated from the members' packages and signed with the group's own key.
If two members hold the same package path or the same
name-epoch:version-release.arch, the first member wins. A group with a proxy
member answers `400`; configure the proxy as its own `.repo` entry instead.

## Client setup

### Hosted or group repository

```bash
sudo tee /etc/yum.repos.d/rpm-hosted.repo <<'EOF'
[rpm-hosted]
name=rpm-hosted
baseurl=https://hangar.example.com/repository/rpm-hosted/
enabled=1
# repomd.xml is signed by Hangar
repo_gpgcheck=1
gpgkey=https://hangar.example.com/repository/rpm-hosted/public.key
# set gpgcheck=1 (and add your key to gpgkey) when your packages are signed
gpgcheck=0
# private repositories
username=<user>
password=<token>
EOF

sudo dnf makecache --repo rpm-hosted
sudo dnf install hello
```

With `repodata_depth: 2`, the `baseurl` includes the directory, for example
`https://hangar.example.com/repository/rpm-hosted/el$releasever/$basearch/`.

### Proxy

```ini
[rocky9-baseos-hangar]
name=Rocky Linux 9 BaseOS through Hangar
baseurl=https://hangar.example.com/repository/rocky9-baseos/
enabled=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9
username=<user>
password=<token>
```

## Upload

```bash
# To a path of your choice (the path is the location in repodata)
curl -u <user>:<token> --upload-file ./hello-2.10-1.el9.x86_64.rpm \
  https://hangar.example.com/repository/rpm-hosted/Packages/h/hello-2.10-1.el9.x86_64.rpm

# Multipart: stored as <directory>/<name>-<version>-<release>.<arch>.rpm
curl -u <user>:<token> -F directory=el9/x86_64/Packages -F file=@hello-2.10-1.el9.x86_64.rpm \
  https://hangar.example.com/repository/rpm-hosted/upload
```

The response is `201` with the stored path and the `baseurl` that serves it.
Paths under `repodata/` are refused. Uploading through a group needs
`write_member`.

## Delete

```bash
curl -u <user>:<token> -X DELETE \
  https://hangar.example.com/repository/rpm-hosted/Packages/h/hello-2.10-1.el9.x86_64.rpm
```

Deleting a version in the web UI, or through cleanup policies, regenerates
repodata too. Quarantined packages leave repodata until they are released.

## Signing keys

The key is generated when the repository is created: RSA 4096, which rpm and
dnf on RHEL/Rocky 8 and 9 accept. Rotate it or import an existing key with
the same admin API as APT repositories:

- `GET /api/v1/repositories/<repo>/signing-key`
- `POST .../signing-key:rotate`
- `PUT .../signing-key`

See [apt.md](apt.md#signing-keys). After a rotation, clients must import the new
key: `sudo rpm --import https://hangar.example.com/repository/rpm-hosted/public.key`,
or `dnf` asks on the next `makecache`.

In the web UI, the **Signing key** panel on the repository settings page shows
the key and links to `public.key` and `public.gpg`, and rotates or imports it;
see [apt.md](apt.md#signing-keys).

## Vulnerability scanning

Rocky Linux, AlmaLinux and Red Hat advisories (OSV `Rocky Linux:9`,
`AlmaLinux:9`, `Red Hat:enterprise_linux:9::baseos`, ...) apply to one
release, so a repository is scanned once its release is known:

- a proxy of a Rocky Linux or AlmaLinux mirror detects it from the major
  version in the remote URL (`https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os`
  is `Rocky Linux:9`), a Red Hat CDN URL from its release and stream;
- otherwise set **Distribution release** in the repository settings, or in
  the API `"config": {"scanning": {"distro": "Rocky Linux:9"}}`. The setting
  wins over the detection.

Packages are checked by their source RPM (`openssl-libs` as `openssl`) with
`[epoch:]version-release`; the fix plan suggests `dnf upgrade <packages>`.
Until the release is known the packages show "No advisory source". Details:
[vulnerabilities.md](vulnerabilities.md#apt-and-rpm-distribution-releases).

## Limits

- One name-epoch-version-release.arch per repodata root: a package stored at
  two paths is listed once (the newest copy), and under `allow_once` the
  second path is refused with `409`.
- The signature of the `repomd.xml` a client read is remembered by the node
  that served it. Behind a load balancer without session affinity, a client
  racing an upload can still get the new signature from another node; the next
  `dnf makecache` succeeds.
- `sqlite` databases, `comps.xml` groups, `updateinfo.xml` and modularity
  metadata are not generated by hosted repositories. A proxy passes them
  through when the remote lists them.
- Packages are not re-signed. Sign them with `rpmsign` before uploading to use
  `gpgcheck=1`.

## Routing rules

A group that combines a hosted repository with a public proxy can serve a
public package published under one of your internal names. Block internal
names on the proxy and reserve them for the hosted member (for example `edcom-*`);
see [routing-rules.md](routing-rules.md).
