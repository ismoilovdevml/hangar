#!/usr/bin/env bash
# Install or upgrade Hangar on one host with Docker Compose, from a published
# release image (nothing is built on the server).
#
#   curl -fsSL https://raw.githubusercontent.com/ismoilovdevml/hangar/main/deploy/install.sh |
#     sudo bash -s -- --domain hangar.example.com
#
# Re-run it to upgrade (--version) or to change a setting; flags you leave out
# keep their current value. Secrets are generated once and never replaced.
# Run with --help for every option. Guide: docs/deploy/install-compose.md.

set -euo pipefail

REPO=ismoilovdevml/hangar
DEFAULT_IMAGE_REPO=ghcr.io/ismoilovdevml/hangar
COMPOSE_FILES="compose.yaml hangar.yaml Caddyfile compose.connectors.example.yaml env.example"
READY_TIMEOUT=180

usage() {
	cat <<'EOF'
Install or upgrade Hangar with Docker Compose.

Usage: install.sh --domain NAME [options]
       install.sh --uninstall [--purge] [--dir DIR]

Options (flags left out keep the value of an existing install):
  --domain NAME            public host name of Hangar (required on first install)
  --version TAG            release to install: v0.1.0 or latest (default: latest
                           on first install, the installed version afterwards)
  --dir DIR                installation directory (default /opt/hangar)
  --bind ADDR              host address Hangar is published on (default 127.0.0.1)
  --port PORT              host port Hangar is published on (default 8080)
  --storage fs|s3          blobs on a local volume (default) or S3
  --s3-endpoint URL        external S3 endpoint; without it --storage s3 runs MinIO.
                           For AWS use --s3-endpoint aws. Credentials, bucket and
                           region go in DIR/.env (HANGAR_STORAGE_S3_*)
  --tls none|caddy         caddy: automatic HTTPS on ports 80/443 (default none:
                           plain HTTP for an existing reverse proxy)
  --registry-domain NAME   separate host for Docker clients (with --tls caddy)
  --acme-email ADDR        e-mail for Let's Encrypt (with --tls caddy)
  --trusted-proxies CIDRS  comma-separated addresses of your reverse proxy
  --base-url URL           external URL (default https://DOMAIN with caddy or
                           --trusted-proxies, else http://DOMAIN:PORT)
  --admin-user NAME        first administrator (default admin)
  --image REF              use this image instead of the release image
  --files-from DIR         take the compose files from DIR instead of downloading
  --uninstall              stop and remove the containers, keep all data
  --purge                  with --uninstall: also delete volumes and DIR
  -h, --help               show this help

Behind an existing reverse proxy (for example a company nginx with TLS):
  install.sh --domain hangar.example.com --tls none --bind 0.0.0.0 \
    --trusted-proxies 10.0.0.5/32
EOF
}

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# ---- Arguments ---------------------------------------------------------------
opt_domain="" opt_version="" opt_dir=/opt/hangar opt_bind="" opt_port="" opt_storage=""
opt_s3_endpoint="" opt_tls="" opt_registry="" opt_acme="" opt_trusted="" opt_base_url=""
opt_admin="" opt_image="" opt_files="" uninstall=0 purge=0

need_value() { [[ $# -ge 2 && -n "$2" ]] || die "$1 needs a value"; }
while [[ $# -gt 0 ]]; do
	case "$1" in
	--domain) need_value "$@"; opt_domain=$2; shift 2 ;;
	--version) need_value "$@"; opt_version=$2; shift 2 ;;
	--dir) need_value "$@"; opt_dir=$2; shift 2 ;;
	--bind) need_value "$@"; opt_bind=$2; shift 2 ;;
	--port) need_value "$@"; opt_port=$2; shift 2 ;;
	--storage) need_value "$@"; opt_storage=$2; shift 2 ;;
	--s3-endpoint) need_value "$@"; opt_s3_endpoint=$2; shift 2 ;;
	--tls) need_value "$@"; opt_tls=$2; shift 2 ;;
	--registry-domain) need_value "$@"; opt_registry=$2; shift 2 ;;
	--acme-email) need_value "$@"; opt_acme=$2; shift 2 ;;
	--trusted-proxies) need_value "$@"; opt_trusted=$2; shift 2 ;;
	--base-url) need_value "$@"; opt_base_url=$2; shift 2 ;;
	--admin-user) need_value "$@"; opt_admin=$2; shift 2 ;;
	--image) need_value "$@"; opt_image=$2; shift 2 ;;
	--files-from) need_value "$@"; opt_files=$2; shift 2 ;;
	--uninstall) uninstall=1; shift ;;
	--purge) purge=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done

dir=${opt_dir%/}
env_file=$dir/.env

# ---- Helpers -----------------------------------------------------------------
compose() { (cd "$dir" && docker compose "$@"); }

# env_get KEY: value of KEY in the .env file, empty when absent.
env_get() {
	[[ -f "$env_file" ]] || return 0
	awk -v k="$1" 'index($0, k "=") == 1 { v = substr($0, length(k) + 2) } END { if (v != "") print v }' "$env_file"
}

# env_set KEY VALUE: replaces KEY's line (also a commented "# KEY=" line) or
# appends it. The value is passed through the environment, so it needs no
# escaping.
env_set() {
	local tmp
	tmp=$(mktemp "$dir/.env.XXXXXX")
	KEY=$1 VALUE=$2 awk '
		BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0 }
		!done && (index($0, k "=") == 1 || index($0, "# " k "=") == 1) { print k "=" v; done = 1; next }
		index($0, k "=") == 1 { next }
		{ print }
		END { if (!done) print k "=" v }
	' "$env_file" >"$tmp"
	chmod 600 "$tmp"
	mv "$tmp" "$env_file"
}

# env_default KEY VALUE: sets KEY only when it is absent or empty.
env_default() {
	[[ -n "$(env_get "$1")" ]] || env_set "$1" "$2"
}

rand_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
rand_b64() { head -c "$1" /dev/urandom | base64 | tr -d '\n'; }

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

download() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }

# ---- Uninstall -----------------------------------------------------------------
if ((uninstall)); then
	[[ -f "$dir/compose.yaml" ]] || die "no Hangar installation in $dir"
	if ((purge)); then
		log "removing containers, volumes (database, blobs) and $dir"
		compose down --volumes --remove-orphans
		rm -rf "$dir"
		log "Hangar and all its data are removed"
	else
		log "stopping and removing containers; volumes and $dir are kept"
		compose down --remove-orphans
		log "data kept: re-run the installer to start again, or --uninstall --purge to delete it"
	fi
	exit 0
fi
((purge)) && die "--purge only works with --uninstall"

# ---- Preconditions -----------------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "docker is not installed (https://docs.docker.com/engine/install/)"
compose_version=$(docker compose version --short 2>/dev/null) ||
	die "the Docker Compose v2 plugin is missing (docker compose version failed)"
compose_version=${compose_version#v}
IFS=. read -r cmaj cmin _ <<<"$compose_version"
if ((cmaj < 2 || (cmaj == 2 && cmin < 20))); then
	die "Docker Compose $compose_version is too old; 2.20 or newer is required"
fi
docker info >/dev/null 2>&1 || die "cannot talk to the Docker daemon (run as root or a member of the docker group)"
command -v curl >/dev/null 2>&1 || die "curl is required"

first_install=0
[[ -f "$env_file" ]] || first_install=1

# Resolve every setting: flag, else the installed value, else the default.
domain=${opt_domain:-$(env_get HANGAR_DOMAIN)}
[[ -n "$domain" ]] || die "--domain is required"
[[ "$domain" =~ ^[A-Za-z0-9.-]+$ ]] || die "--domain $domain: want a host name"
bind=${opt_bind:-$(env_get HANGAR_BIND_ADDR)}
bind=${bind:-127.0.0.1}
port=${opt_port:-$(env_get HANGAR_HTTP_PORT)}
port=${port:-8080}
if ! [[ "$port" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
	die "--port $port: want 1-65535"
fi
storage=${opt_storage:-$(env_get HANGAR_STORAGE_KIND)}
((first_install)) && storage=${opt_storage:-fs}
[[ "$storage" == fs || "$storage" == s3 ]] || die "--storage must be fs or s3"
tls=$opt_tls
if [[ -z "$tls" ]]; then
	tls=none
	[[ ",$(env_get COMPOSE_PROFILES)," == *",caddy,"* ]] && tls=caddy
fi
[[ "$tls" == none || "$tls" == caddy ]] || die "--tls must be none or caddy"
registry=${opt_registry:-$(env_get HANGAR_REGISTRY_DOMAIN)}
((first_install)) && registry=$opt_registry
trusted=${opt_trusted:-$(env_get HANGAR_INSTALL_TRUSTED_PROXIES)}
acme=${opt_acme:-$(env_get ACME_EMAIL)}
if [[ "$tls" == caddy && (-z "$acme" || "$acme" == ops@example.com) ]]; then
	die "--tls caddy needs --acme-email (Let's Encrypt account and expiry notices)"
fi
admin=${opt_admin:-$(env_get HANGAR_BOOTSTRAP_ADMIN_USER)}
admin=${admin:-admin}

s3_endpoint=$opt_s3_endpoint
if [[ "$storage" == s3 && -z "$s3_endpoint" && $first_install -eq 0 ]]; then
	current=$(env_get HANGAR_STORAGE_S3_ENDPOINT)
	[[ ",$(env_get COMPOSE_PROFILES)," == *",minio,"* ]] || s3_endpoint=${current:-aws}
fi
s3_endpoint_value=$s3_endpoint
[[ "$s3_endpoint" == aws ]] && s3_endpoint_value=""
use_minio=0
[[ "$storage" == s3 && -z "$s3_endpoint" ]] && use_minio=1

if [[ -n "$opt_base_url" ]]; then
	base_url=${opt_base_url%/}
elif [[ "$tls" == caddy || -n "$trusted" ]]; then
	base_url=https://$domain
elif [[ -n "$opt_domain$opt_port" || $first_install -eq 1 ]]; then
	base_url=http://$domain:$port
else
	base_url=$(env_get HANGAR_BASE_URL)
fi
[[ "$base_url" =~ ^https?://[^/]+$ ]] || die "--base-url $base_url: want scheme://host[:port]"

# Version and image.
version=$opt_version
if [[ -z "$version" ]]; then
	version=$(env_get HANGAR_VERSION)
	version=${version:-latest}
fi
if [[ "$version" == latest ]]; then
	if [[ -n "$opt_files" && -n "$opt_image" ]]; then
		version=local
	else
		log "looking up the latest release"
		version=$(curl -fsSL --retry 3 "https://api.github.com/repos/$REPO/releases/latest" |
			sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) || true
		[[ -n "$version" ]] || die "could not find the latest release of $REPO; pass --version vX.Y.Z"
	fi
fi
if [[ -n "$opt_image" ]]; then
	image=$opt_image
elif [[ -n "$opt_version" || $first_install -eq 1 || -z "$(env_get HANGAR_IMAGE)" ]]; then
	image=$DEFAULT_IMAGE_REPO:${version#v}
else
	image=$(env_get HANGAR_IMAGE)
fi

if [[ "$EUID" -ne 0 && ! -w "$(dirname "$dir")" && ! -d "$dir" ]]; then
	die "cannot create $dir: run as root (sudo) or choose --dir"
fi

# ---- Files -----------------------------------------------------------------------
log "installing Hangar $version into $dir"
install -d -m 0755 "$dir" "$dir/sites"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

fetch_files() {
	local f src
	if [[ -n "$opt_files" ]]; then
		for f in $COMPOSE_FILES; do
			src=$opt_files/$f
			[[ "$f" == env.example ]] && src=$opt_files/.env.example
			cp "$src" "$stage/$f"
		done
		cp "$opt_files/sites/registry.caddy" "$stage/registry.caddy"
		return
	fi
	local base=https://github.com/$REPO/releases/download/$version
	if download "$base/SHA256SUMS" "$stage/SHA256SUMS" 2>/dev/null; then
		for f in $COMPOSE_FILES registry.caddy; do
			download "$base/$f" "$stage/$f" || die "cannot download $f from release $version"
			want=$(awk -v f="$f" '$2 == f || $2 == "*" f { print $1 }' "$stage/SHA256SUMS")
			[[ -n "$want" && "$want" == "$(sha256_of "$stage/$f")" ]] || die "checksum mismatch for $f in release $version"
		done
	else
		warn "release $version has no SHA256SUMS asset; using the files of tag $version"
		local raw=https://raw.githubusercontent.com/$REPO/$version/deploy/compose
		for f in $COMPOSE_FILES; do
			src=$f
			[[ "$f" == env.example ]] && src=.env.example
			download "$raw/$src" "$stage/$f" || die "cannot download deploy/compose/$src for $version"
		done
		download "$raw/sites/registry.caddy" "$stage/registry.caddy" || die "cannot download sites/registry.caddy"
	fi
}
fetch_files

# Release-owned files are replaced; files you may edit are kept and the new
# version is written next to them as *.new when it differs.
install -m 0644 "$stage/compose.yaml" "$dir/compose.yaml"
install -m 0644 "$stage/compose.connectors.example.yaml" "$dir/compose.connectors.example.yaml"
install -m 0644 "$stage/env.example" "$dir/env.example"
for f in hangar.yaml Caddyfile; do
	if [[ ! -f "$dir/$f" ]]; then
		install -m 0644 "$stage/$f" "$dir/$f"
	elif ! cmp -s "$stage/$f" "$dir/$f"; then
		install -m 0644 "$stage/$f" "$dir/$f.new"
		warn "$dir/$f differs from the $version version; review $dir/$f.new"
	fi
done
if [[ "$tls" == caddy && -n "$registry" ]]; then
	install -m 0644 "$stage/registry.caddy" "$dir/sites/registry.caddy"
else
	rm -f "$dir/sites/registry.caddy"
fi

# ---- .env ------------------------------------------------------------------------
if ((first_install)); then
	(umask 077 && cp "$stage/env.example" "$env_file")
fi
chmod 600 "$env_file"

profiles=postgres
((use_minio)) && profiles=$profiles,minio
[[ "$tls" == caddy ]] && profiles=$profiles,caddy
subnet=$(env_get HANGAR_NETWORK_SUBNET)
subnet=${subnet:-172.30.80.0/24}
trusted_all=$subnet
[[ -n "$trusted" ]] && trusted_all=$trusted,$subnet

env_set COMPOSE_PROFILES "$profiles"
env_set HANGAR_VERSION "$version"
env_set HANGAR_IMAGE "$image"
env_set HANGAR_DOMAIN "$domain"
env_set HANGAR_BASE_URL "$base_url"
env_set HANGAR_BIND_ADDR "$bind"
env_set HANGAR_HTTP_PORT "$port"
env_set HANGAR_NETWORK_SUBNET "$subnet"
env_set HANGAR_INSTALL_TRUSTED_PROXIES "$trusted"
env_set HANGAR_TRUSTED_PROXIES "$trusted_all"
env_set HANGAR_STORAGE_KIND "$storage"
env_set HANGAR_REGISTRY_DOMAIN "$registry"
[[ -n "$acme" ]] && env_set ACME_EMAIL "$acme"
if [[ "$storage" == s3 ]]; then
	if ((use_minio)); then
		env_set HANGAR_STORAGE_S3_ENDPOINT http://minio:9000
		env_set HANGAR_STORAGE_S3_USE_PATH_STYLE true
		env_default HANGAR_STORAGE_S3_ACCESS_KEY_ID hangar
		env_default HANGAR_STORAGE_S3_SECRET_ACCESS_KEY "$(rand_hex 24)"
		env_default MINIO_ROOT_PASSWORD "$(rand_hex 24)"
	elif [[ -n "$opt_s3_endpoint" ]]; then
		env_set HANGAR_STORAGE_S3_ENDPOINT "$s3_endpoint_value"
		[[ "$s3_endpoint" == aws ]] && env_set HANGAR_STORAGE_S3_USE_PATH_STYLE false
	fi
fi

# Secrets: generated once, never replaced.
env_default HANGAR_SECRET_KEY "$(rand_b64 32)"
env_default HANGAR_DOCKER_TOKEN_KEY "$(rand_b64 48)"
env_default POSTGRES_PASSWORD "$(rand_hex 24)"
if ((first_install)); then
	env_set HANGAR_BOOTSTRAP_ADMIN_USER "$admin"
	env_set HANGAR_BOOTSTRAP_ADMIN_PASSWORD "$(rand_hex 12)"
elif [[ -n "$opt_admin" ]]; then
	env_set HANGAR_BOOTSTRAP_ADMIN_USER "$admin"
fi

if [[ "$storage" == s3 && $use_minio -eq 0 ]]; then
	if [[ -z "$(env_get HANGAR_STORAGE_S3_BUCKET)" || -z "$(env_get HANGAR_STORAGE_S3_SECRET_ACCESS_KEY)" ]]; then
		die "set HANGAR_STORAGE_S3_BUCKET, HANGAR_STORAGE_S3_REGION, HANGAR_STORAGE_S3_ACCESS_KEY_ID and HANGAR_STORAGE_S3_SECRET_ACCESS_KEY in $env_file, then run the installer again"
	fi
fi

# ---- Start -----------------------------------------------------------------------
compose config --quiet || die "the compose configuration is invalid (see above)"
log "pulling images"
services=()
while IFS= read -r svc; do
	[[ "$svc" == hangar ]] || services+=("$svc")
done < <(compose config --services)
((${#services[@]} == 0)) || compose pull --quiet "${services[@]}"
# The Hangar image may also be a locally built one (--image): use the local
# copy when it cannot be pulled.
if ! compose pull --quiet hangar; then
	docker image inspect "$image" >/dev/null 2>&1 || die "cannot pull $image"
	warn "could not pull $image; using the local image"
fi
log "starting"
compose up -d --remove-orphans

probe_host=$bind
[[ "$probe_host" == 0.0.0.0 || "$probe_host" == "::" ]] && probe_host=127.0.0.1
[[ "$probe_host" == *:* ]] && probe_host="[$probe_host]"
log "waiting for http://$probe_host:$port/readyz (up to ${READY_TIMEOUT}s)"
deadline=$((SECONDS + READY_TIMEOUT))
until curl -fsS -o /dev/null --max-time 5 "http://$probe_host:$port/readyz" 2>/dev/null; do
	if ((SECONDS >= deadline)); then
		compose ps
		compose logs --tail 40 hangar
		die "Hangar did not become ready within ${READY_TIMEOUT}s (logs above)"
	fi
	sleep 3
done

installed=$(curl -fsS --max-time 5 "http://$probe_host:$port/api/v1/system/status" |
	sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -n 1) || true

cat <<EOF

Hangar ${installed:-$version} is running.

  URL:            $base_url
  Local address:  http://$probe_host:$port
  Configuration:  $env_file (secrets, mode 600) and $dir/hangar.yaml
EOF
if ((first_install)); then
	cat <<EOF
  Administrator:  $admin
  Password:       sudo grep HANGAR_BOOTSTRAP_ADMIN_PASSWORD $env_file

Next steps:
  1. Sign in and change the administrator password.
  2. Back up $env_file: HANGAR_SECRET_KEY decrypts credentials stored in
     the database. Backups: docs/deploy/backup-restore.md.
EOF
	if [[ "$tls" == none ]]; then
		cat <<EOF
  3. Point your reverse proxy (TLS) at $bind:$port and make sure it is in
     --trusted-proxies; settings for large uploads: docs/deploy/reverse-proxy.md.
EOF
	fi
fi
cat <<EOF

Upgrade later with:  install.sh --version vX.Y.Z   (or --version latest)
EOF
