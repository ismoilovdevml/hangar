#!/usr/bin/env bash
# Install, upgrade or remove Hangar on one host with Docker Compose.
#
#   curl -fsSL https://github.com/ismoilovdevml/hangar/releases/latest/download/install.sh | sudo bash
#
# It asks for a data directory and an optional domain, generates every
# secret, starts Hangar and PostgreSQL (and Caddy for automatic TLS) and
# prints the address and the first administrator's one-time password.
# Re-run it to upgrade. Run with --help for unattended use.
# Guide: docs/deploy/install-compose.md.

set -euo pipefail

# The whole script is one block: bash reads all of it before running any of
# it, so a truncated download never runs, and commands that read stdin (the
# script itself under `curl | bash`) cannot swallow the rest.
{

REPO=ismoilovdevml/hangar
DEFAULT_IMAGE_REPO=ghcr.io/ismoilovdevml/hangar
DEFAULT_DATA_DIR=/srv/hangar
# Installations made by earlier versions of this script.
LEGACY_DIR=/opt/hangar
# Members of the release asset hangar-compose.tar.gz.
STACK_FILES="compose.yaml hangar.yaml Caddyfile registry.caddy compose.connectors.example.yaml"
# Container users: Hangar's image runs as 65532 (Dockerfile USER), the
# postgres alpine image as 70.
HANGAR_UID=65532
POSTGRES_UID=70
# Seconds to wait for /readyz; a slow first start may need more.
READY_TIMEOUT=${HANGAR_INSTALL_READY_TIMEOUT:-180}

usage() {
	cat <<'EOF'
Install or upgrade Hangar with Docker Compose.

Usage: install.sh [--data-dir DIR] [--domain NAME] [options]
       install.sh --migrate-from /opt/hangar --data-dir DIR
       install.sh --uninstall [--purge] [--data-dir DIR]

Without flags it asks for the data directory and the domain. Re-run it to
upgrade: data, secrets and settings are kept; flags change a setting.

  --data-dir DIR           where everything is stored (asked; default /srv/hangar);
                           required without a terminal
  --domain NAME            public host name; without one Hangar is served at
                           http://<host-ip>:<port>
  --tls caddy|proxy        with a domain: caddy gets a certificate itself
                           (ports 80/443); proxy runs behind your reverse proxy,
                           which terminates TLS (default caddy)
  --port PORT              host port of Hangar's HTTP listener (default 8080)
  -y, --yes                do not ask; use flags and defaults

Advanced:
  --version TAG            release to install, e.g. v0.2.0 (default latest)
  --bind ADDR              host address the port is published on (default
                           127.0.0.1 with caddy, else 0.0.0.0)
  --trusted-proxies CIDRS  your reverse proxy's addresses, comma-separated
  --base-url URL           external URL when it is neither https://DOMAIN nor
                           http://<host-ip>:<port>
  --acme-email ADDR        e-mail for certificate expiry notices (caddy)
  --registry-domain NAME   separate host name for Docker clients (caddy)
  --image REF              Hangar image instead of the release image
  --files-from PATH        compose files from a directory or hangar-compose.tar.gz
                           instead of downloading them (offline installs)
  --mirror URL             download release files from URL/<version>/ (an internal
                           copy of the GitHub release assets); needs --version
  --migrate-from DIR       move an installation in the old layout (DIR/.env and
                           Docker volumes) into --data-dir; the old one is kept
  --uninstall              stop and remove the containers; keep the data
  --purge                  with --uninstall: also delete the data directory
  -h, --help               show this help
EOF
}

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# ---- Arguments ------------------------------------------------------------------
opt_data="" opt_domain="" opt_tls="" opt_port="" opt_yes=0 opt_version="" opt_bind=""
opt_trusted="" opt_base_url="" opt_acme="" opt_registry="" opt_image="" opt_files=""
opt_mirror="" opt_migrate="" uninstall=0 purge=0

need_value() { [[ $# -ge 2 && -n "$2" ]] || die "$1 needs a value"; }
while [[ $# -gt 0 ]]; do
	case "$1" in
	--data-dir) need_value "$@"; opt_data=$2; shift 2 ;;
	--domain) need_value "$@"; opt_domain=$2; shift 2 ;;
	--tls) need_value "$@"; opt_tls=$2; shift 2 ;;
	--port) need_value "$@"; opt_port=$2; shift 2 ;;
	-y | --yes) opt_yes=1; shift ;;
	--version) need_value "$@"; opt_version=$2; shift 2 ;;
	--bind) need_value "$@"; opt_bind=$2; shift 2 ;;
	--trusted-proxies) need_value "$@"; opt_trusted=$2; shift 2 ;;
	--base-url) need_value "$@"; opt_base_url=${2%/}; shift 2 ;;
	--acme-email) need_value "$@"; opt_acme=$2; shift 2 ;;
	--registry-domain) need_value "$@"; opt_registry=$2; shift 2 ;;
	--image) need_value "$@"; opt_image=$2; shift 2 ;;
	--files-from) need_value "$@"; opt_files=${2%/}; shift 2 ;;
	--mirror) need_value "$@"; opt_mirror=${2%/}; shift 2 ;;
	--migrate-from) need_value "$@"; opt_migrate=${2%/}; shift 2 ;;
	--uninstall) uninstall=1; shift ;;
	--purge) purge=1; shift ;;
	-h | --help) usage; exit 0 ;;
	--dir) die "--dir is gone: use --data-dir for a new installation, or --migrate-from $LEGACY_DIR --data-dir $DEFAULT_DATA_DIR to move an existing one" ;;
	--storage | --s3-endpoint | --admin-user) die "$1 is no longer supported (see docs/deploy/install-compose.md)" ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done
((purge && !uninstall)) && die "--purge only works with --uninstall"
case "$opt_tls" in
"" | caddy | proxy) ;;
none) opt_tls=proxy ;;
*) die "--tls must be caddy or proxy" ;;
esac

# ---- Terminal ---------------------------------------------------------------------
# Questions are read from the terminal, not stdin, which is the script itself
# under `curl | bash`.
interactive=0
if ((!opt_yes)) && { exec 3<"${HANGAR_INSTALL_TTY:-/dev/tty}"; } 2>/dev/null; then
	interactive=1
fi

# ask PROMPT DEFAULT: prints the answer, or DEFAULT for an empty one.
ask() {
	local answer=""
	printf '%s' "$1" >&2
	IFS= read -r answer <&3 || true
	answer=${answer#"${answer%%[![:space:]]*}"}
	answer=${answer%"${answer##*[![:space:]]}"}
	printf '%s' "${answer:-$2}"
}

# confirm PROMPT: true unless the answer starts with n.
confirm() {
	((interactive)) || return 0
	local a
	a=$(ask "$1 [Y/n] " y)
	[[ "$a" != [nN]* ]]
}

# ---- Helpers ----------------------------------------------------------------------
# env_get FILE KEY: KEY's value in FILE (surrounding quotes removed), empty when
# absent.
env_get() {
	[[ -f "$1" ]] || return 0
	awk -v k="$2" '
		index($0, k "=") == 1 { v = substr($0, length(k) + 2) }
		END {
			if (v ~ /^".*"$/ || v ~ /^'\''.*'\''$/) v = substr(v, 2, length(v) - 2)
			if (v != "") print v
		}' "$1"
}

# env_set FILE KEY VALUE: replaces KEY's line or appends it, keeping every
# other line. The value travels through the environment: no escaping needed.
env_set() {
	local tmp
	tmp=$(mktemp "$1.XXXXXX")
	KEY=$2 VALUE=$3 awk '
		BEGIN { k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0 }
		!done && index($0, k "=") == 1 { print k "=" v; done = 1; next }
		index($0, k "=") == 1 { next }
		{ print }
		END { if (!done) print k "=" v }
	' "$1" >"$tmp"
	chmod 644 "$tmp"
	mv "$tmp" "$1"
}

rand_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
rand_b64() { head -c "$1" /dev/urandom | base64 | tr -d '\n'; }

# urlencode STRING: percent-encodes everything but unreserved characters.
urlencode() {
	local s=$1 out="" c i
	for ((i = 0; i < ${#s}; i++)); do
		c=${s:i:1}
		case "$c" in
		[A-Za-z0-9._~-]) out+=$c ;;
		*) out+=$(printf '%%%02X' "'$c") ;;
		esac
	done
	printf '%s' "$out"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

download() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }

host_ip() {
	local ip=""
	if command -v ip >/dev/null 2>&1; then
		ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }') || true
	fi
	[[ -n "$ip" ]] || ip=$(hostname -I 2>/dev/null | awk '{ print $1 }') || true
	printf '%s' "${ip:-127.0.0.1}"
}

# Working directory of the running "hangar" container's compose project.
running_project_dir() {
	docker inspect -f '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' hangar 2>/dev/null || true
}

is_installed() { [[ -f "$1/.env" && -f "$1/compose.yaml" && -d "$1/secrets" ]]; }
is_legacy() { [[ -f "$1/.env" && -f "$1/compose.yaml" && ! -d "$1/secrets" ]] && grep -q '^HANGAR_SECRET_KEY=.' "$1/.env"; }

compose() { (cd "$data" && docker compose "$@"); }

# write_secret NAME VALUE UID: creates secrets/NAME (mode 600) unless it
# exists. Secrets are generated once and never replaced.
write_secret() {
	local f=$data/secrets/$1 tmp=$data/secrets/.$1.tmp
	[[ -e "$f" ]] && return 0
	# The server skips dot files: it never reads a half-written one.
	(umask 077 && printf '%s\n' "$2" >"$tmp")
	chown "$3:$3" "$tmp"
	mv "$tmp" "$f"
}

# make_dir PATH MODE UID: creates PATH and sets its mode and owner.
make_dir() {
	mkdir -p "$1"
	chmod "$2" "$1"
	chown "$3:$3" "$1"
}

# ---- Preconditions ----------------------------------------------------------------
[[ "$(id -u)" -eq 0 ]] || die "run as root (sudo)"
command -v docker >/dev/null 2>&1 || die "docker is not installed (https://docs.docker.com/engine/install/)"
compose_version=$(docker compose version --short 2>/dev/null) ||
	die "the Docker Compose v2 plugin is missing (docker compose version failed)"
compose_version=${compose_version#v}
IFS=. read -r cmaj cmin _ <<<"$compose_version"
if ((cmaj < 2 || (cmaj == 2 && cmin < 20))); then
	die "Docker Compose $compose_version is too old; 2.20 or newer is required"
fi
docker info >/dev/null 2>&1 || die "cannot talk to the Docker daemon"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"

# ---- Uninstall --------------------------------------------------------------------
if ((uninstall)); then
	data=$opt_data
	if [[ -z "$data" ]]; then
		guess=$(running_project_dir)
		is_installed "$guess" || guess=$DEFAULT_DATA_DIR
		((interactive)) || die "--data-dir is required without a terminal"
		data=$(ask "Data directory of the installation [$guess]: " "$guess")
	fi
	data=${data%/}
	is_installed "$data" || die "no Hangar installation in $data"
	log "stopping and removing the containers"
	compose down --remove-orphans
	if ((!purge)); then
		log "Hangar is stopped; all data is kept in $data"
		echo "Start it again by re-running the installer with --data-dir $data."
		exit 0
	fi
	if ((interactive)); then
		echo "This deletes $data: the database, every package and the secrets." >&2
		[[ "$(ask "Type the data directory to confirm: " "")" == "$data" ]] || die "not confirmed; nothing deleted"
	elif ((!opt_yes)); then
		die "--purge without a terminal needs --yes"
	fi
	rm -rf -- "$data"
	log "Hangar and all its data are deleted"
	exit 0
fi

# ---- Data directory and mode ------------------------------------------------------
running=$(running_project_dir)
data=$opt_data
if [[ -z "$data" ]]; then
	((interactive)) || die "--data-dir is required without a terminal"
	suggest=$DEFAULT_DATA_DIR
	is_installed "$running" && suggest=$running
	data=$(ask "Data directory [$suggest]: " "$suggest")
fi
data=${data%/}
[[ "$data" == /* && "$data" != / ]] || die "the data directory must be an absolute path other than /"
is_legacy "$data" && die "$data holds an installation in the old layout; move it with --migrate-from $data --data-dir $DEFAULT_DATA_DIR"

installed=0
is_installed "$data" && installed=1
[[ -e "$data/.migrating" ]] &&
	die "a migration into $data did not finish: start the old installation again if it is stopped (docker compose up -d in its directory), delete $data and run the migration again"

legacy=$opt_migrate
if [[ -n "$legacy" ]] && ((installed)); then
	warn "$data is already installed; upgrading it and ignoring --migrate-from"
	legacy=""
fi
if [[ -z "$legacy" ]] && ((!installed)); then
	for cand in "$running" "$LEGACY_DIR"; do
		if [[ -z "$cand" ]] || ! is_legacy "$cand"; then continue; fi
		if ((interactive)) && confirm "Hangar is installed in $cand (old layout). Move it into $data?"; then
			legacy=$cand
			break
		fi
		die "Hangar is installed in $cand (old layout). Move it with --migrate-from $cand --data-dir $data"
	done
fi
if [[ -n "$legacy" ]]; then
	legacy=${legacy%/}
	is_legacy "$legacy" || die "$legacy has no installation in the old layout (.env and compose.yaml)"
	[[ "$data" != "$legacy" && "$data" != "$legacy"/* ]] || die "--data-dir must be outside $legacy"
fi
if ((!installed)) && [[ -d "$data" && -n "$(ls -A "$data")" ]]; then
	die "$data exists and is not empty; choose an empty or new directory"
fi
if [[ -n "$running" && "$running" != "$data" && "$running" != "$legacy" ]]; then
	die "another Hangar installation runs from $running; uninstall it first (--uninstall --data-dir $running)"
fi

state=""
((installed)) && state=$data/.env
[[ -n "$legacy" ]] && state=$legacy/.env
stored() { [[ -n "$state" ]] && env_get "$state" "$1"; }

# ---- Settings ---------------------------------------------------------------------
domain=$opt_domain
if [[ -z "$domain" && -n "$state" ]]; then domain=$(stored HANGAR_DOMAIN || true); fi
if [[ -z "$domain" && -z "$state" ]] && ((interactive)); then
	domain=$(ask "Domain name (empty: http://$(host_ip):${opt_port:-8080}): " "")
fi
[[ -z "$domain" || "$domain" =~ ^[A-Za-z0-9.-]+$ ]] || die "domain $domain: want a host name"

tls=$opt_tls
if [[ -z "$tls" && -n "$domain" ]]; then
	if ((installed)); then
		tls=$(stored HANGAR_TLS || true)
	elif [[ -n "$legacy" ]]; then
		tls=proxy
		[[ ",$(stored COMPOSE_PROFILES)," == *",caddy,"* ]] && tls=caddy
	elif ((interactive)); then
		cat >&2 <<EOF
How does $domain get HTTPS?
  1) Hangar gets a certificate itself (Let's Encrypt; DNS points here, ports 80 and 443 open)
  2) An existing reverse proxy terminates TLS and forwards to Hangar
EOF
		case "$(ask "Choose [1]: " 1)" in
		1) tls=caddy ;;
		2) tls=proxy ;;
		*) die "answer 1 or 2" ;;
		esac
	fi
	tls=${tls:-caddy}
fi
[[ -n "$domain" || -z "$tls" ]] || die "--tls needs --domain"

port=${opt_port:-$(stored HANGAR_HTTP_PORT || true)}
port=${port:-8080}
if ! [[ "$port" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
	die "--port $port: want 1-65535"
fi

bind=$opt_bind
if [[ -z "$bind" && -z "$opt_tls" ]]; then bind=$(stored HANGAR_BIND_ADDR || true); fi
if [[ -z "$bind" ]]; then
	bind=0.0.0.0
	[[ "$tls" == caddy ]] && bind=127.0.0.1
fi

trusted=${opt_trusted:-$(stored HANGAR_INSTALL_TRUSTED_PROXIES || true)}
acme=${opt_acme:-$(stored ACME_EMAIL || true)}
[[ "$acme" == ops@example.com ]] && acme=""
registry=""
if [[ "$tls" == caddy ]]; then
	registry=${opt_registry:-$(stored HANGAR_REGISTRY_DOMAIN || true)}
	[[ "$registry" == registry.example.com ]] && registry=""
elif [[ -n "$opt_registry" ]]; then
	die "--registry-domain needs --tls caddy"
fi

if [[ -n "$opt_base_url" ]]; then
	base_url=$opt_base_url
elif [[ -n "$state" && -z "$opt_domain$opt_tls$opt_port" && -n "$(stored HANGAR_BASE_URL || true)" ]]; then
	base_url=$(stored HANGAR_BASE_URL)
elif [[ -n "$domain" ]]; then
	base_url=https://$domain
else
	base_url=http://$(host_ip):$port
fi
[[ "$base_url" =~ ^https?://[^/]+$ ]] || die "--base-url $base_url: want scheme://host[:port]"
insecure=false
[[ "$base_url" == http://* ]] && insecure=true

subnet=$(stored HANGAR_NETWORK_SUBNET || true)
subnet=${subnet:-172.30.80.0/24}
pg_user=$(stored POSTGRES_USER || true)
pg_user=${pg_user:-hangar}
pg_db=$(stored POSTGRES_DB || true)
pg_db=${pg_db:-hangar}
previous_version=""
((installed)) && previous_version=$(stored HANGAR_VERSION || true)
# First run: no earlier run of the installer on this directory got Hangar
# ready (HANGAR_INSTALLED is written once it did).
fresh=0
if [[ -z "$legacy" ]] && [[ -z "$(stored HANGAR_INSTALLED || true)" ]]; then fresh=1; fi

# Version and image.
version=${opt_version:-latest}
if [[ "$version" == latest ]]; then
	if [[ -n "$opt_files" && -n "$opt_image" ]]; then
		version=local
	else
		[[ -z "$opt_mirror" ]] || die "--mirror needs --version"
		log "looking up the latest release"
		version=$(curl -fsSL --retry 3 "https://api.github.com/repos/$REPO/releases/latest" |
			sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) || true
		[[ -n "$version" ]] || die "could not find the latest release of $REPO; pass --version vX.Y.Z"
	fi
fi
image=${opt_image:-$DEFAULT_IMAGE_REPO:${version#v}}

# ---- Plan -------------------------------------------------------------------------
if ((installed)); then
	action="Upgrade Hangar in $data to $version"
elif [[ -n "$legacy" ]]; then
	action="Move Hangar from $legacy into $data and install $version (the old installation is kept for rollback)"
else
	action="Install Hangar $version into $data"
fi
cat <<EOF

$action
  URL:   $base_url
  Port:  $bind:$port${tls:+ (TLS: $tls)}
EOF
[[ -n "$trusted" ]] && echo "  Trusted proxies: $trusted"
echo
confirm "Continue?" || die "cancelled"

# ---- Release files ----------------------------------------------------------------
stage=$(mktemp -d)
migrating=0
# shellcheck disable=SC2329 # run by the EXIT trap
on_exit() {
	local rc=$?
	rm -rf "$stage"
	if ((rc != 0)) && [[ -n "$legacy" && $migrating -eq 0 && -e "$data/.migrating" ]]; then
		printf '\nThe old installation was not stopped. Delete %s before running the migration again.\n' "$data" >&2
	elif ((rc != 0 && migrating)); then
		cat >&2 <<EOF

The migration did not finish. The old installation and its volumes are
unchanged; to return to it:

  cd $data && docker compose down
  cd $legacy && docker compose up -d

Then delete $data before running the migration again.
EOF
	fi
}
trap on_exit EXIT

fetch_stack() {
	local f
	if [[ -f "$opt_files" ]]; then
		# shellcheck disable=SC2086
		tar -xzf "$opt_files" -C "$stage" $STACK_FILES || die "cannot unpack $opt_files"
		return
	fi
	if [[ -n "$opt_files" ]]; then
		for f in compose.yaml hangar.yaml Caddyfile compose.connectors.example.yaml; do
			cp "$opt_files/$f" "$stage/$f" || die "$opt_files/$f is missing"
		done
		f=$opt_files/registry.caddy
		[[ -f "$f" ]] || f=$opt_files/sites/registry.caddy
		cp "$f" "$stage/registry.caddy" || die "$opt_files has no registry.caddy"
		return
	fi
	local base=https://github.com/$REPO/releases/download/$version want
	[[ -n "$opt_mirror" ]] && base=$opt_mirror/$version
	download "$base/SHA256SUMS" "$stage/SHA256SUMS" || die "cannot download $base/SHA256SUMS"
	want=$(awk '$2 == "hangar-compose.tar.gz" || $2 == "*hangar-compose.tar.gz" { print $1 }' "$stage/SHA256SUMS")
	[[ -n "$want" ]] || die "release $version has no hangar-compose.tar.gz; it predates this installer (use that release's install.sh)"
	download "$base/hangar-compose.tar.gz" "$stage/hangar-compose.tar.gz" ||
		die "cannot download hangar-compose.tar.gz from release $version"
	[[ "$want" == "$(sha256_of "$stage/hangar-compose.tar.gz")" ]] ||
		die "checksum mismatch for hangar-compose.tar.gz in release $version"
	# shellcheck disable=SC2086
	tar -xzf "$stage/hangar-compose.tar.gz" -C "$stage" $STACK_FILES ||
		die "release $version uses an older layout of hangar-compose.tar.gz; use that release's install.sh"
}
log "fetching the compose files of $version"
fetch_stack
grep -q HANGAR_SECRETS_DIR "$stage/compose.yaml" ||
	die "release $version uses the old layout (.env with secrets); use that release's install.sh"

# ---- Migration checks (before anything is stopped) ---------------------------------
if [[ -n "$legacy" ]]; then
	project=$(stored COMPOSE_PROJECT_NAME || true)
	project=${project:-hangar}
	vol_data=${project}_hangar-data
	vol_pg=${project}_postgres-data
	kind=$(stored HANGAR_STORAGE_KIND || true)
	[[ "${kind:-fs}" == fs ]] || die "$legacy stores packages in S3; the migration only moves local storage (fs)"
	profiles=",$(stored COMPOSE_PROFILES || true),"
	[[ "$profiles" == *",postgres,"* ]] || die "$legacy uses an external database; the migration only moves the bundled PostgreSQL"
	[[ -n "$(stored POSTGRES_PASSWORD || true)" ]] || die "$legacy/.env has no POSTGRES_PASSWORD"
	for v in "$vol_pg" "$vol_data"; do
		docker volume inspect "$v" >/dev/null 2>&1 || die "Docker volume $v of $legacy does not exist"
	done
	if [[ -f "$legacy/compose.override.yaml" ]]; then
		warn "$legacy/compose.override.yaml is not migrated; re-create what you need in $data/compose.override.yaml"
	fi
fi

# ---- Layout -----------------------------------------------------------------------
mkdir -p "$data"
chmod 755 "$data"
[[ -n "$legacy" ]] && : >"$data/.migrating"
make_dir "$data/config" 755 0
make_dir "$data/secrets" 700 "$HANGAR_UID"
make_dir "$data/blobs" 750 "$HANGAR_UID"
make_dir "$data/nats" 750 "$HANGAR_UID"
make_dir "$data/postgres" 700 "$POSTGRES_UID"
make_dir "$data/backups" 700 0

if [[ -n "$legacy" ]]; then
	# Secrets of the old .env become files; the rest of it stays behind.
	# The long name wins over the HANGAR_SECRET_KEY alias, as in the server.
	for pair in HANGAR_SECURITY_SECRET_KEY:security.secret_key HANGAR_SECRET_KEY:security.secret_key \
		HANGAR_SECURITY_PREVIOUS_SECRET_KEYS:security.previous_secret_keys \
		HANGAR_DOCKER_TOKEN_KEY:docker.token_key \
		HANGAR_AUTH_OIDC_CLIENT_SECRET:auth.oidc.client_secret \
		HANGAR_AUTH_LDAP_BIND_PASSWORD:auth.ldap.bind_password \
		HANGAR_HTTP_PROXY:http.proxy HANGAR_HTTP_PROXY_USERNAME:http.proxy_username \
		HANGAR_HTTP_PROXY_PASSWORD:http.proxy_password HANGAR_HTTP_NO_PROXY:http.no_proxy; do
		value=$(stored "${pair%%:*}" || true)
		[[ -z "$value" ]] || write_secret "${pair#*:}" "$value" "$HANGAR_UID"
	done
	pg_password=$(stored POSTGRES_PASSWORD)
else
	write_secret security.secret_key "$(rand_b64 32)" "$HANGAR_UID"
	write_secret docker.token_key "$(rand_b64 48)" "$HANGAR_UID"
	pg_password=$(rand_hex 24)
fi
write_secret postgres-password "$pg_password" "$POSTGRES_UID"
pg_password=$(<"$data/secrets/postgres-password")
write_secret database.url "postgres://$(urlencode "$pg_user"):$(urlencode "$pg_password")@postgres:5432/$(urlencode "$pg_db")?sslmode=disable" "$HANGAR_UID"
# Release-owned files are replaced on every run; config/hangar.yaml is yours.
install -m 0644 "$stage/compose.yaml" "$data/compose.yaml"
install -m 0644 "$stage/compose.connectors.example.yaml" "$data/compose.connectors.example.yaml"
if [[ ! -f "$data/config/hangar.yaml" ]]; then
	if [[ -n "$legacy" && -f "$legacy/hangar.yaml" ]]; then
		install -m 0644 "$legacy/hangar.yaml" "$data/config/hangar.yaml"
	else
		install -m 0644 "$stage/hangar.yaml" "$data/config/hangar.yaml"
	fi
fi
if [[ "$tls" == caddy ]]; then
	make_dir "$data/caddy" 700 0
	for d in sites data config; do make_dir "$data/caddy/$d" 700 0; done
	if [[ -n "$acme" ]]; then
		install -m 0644 "$stage/Caddyfile" "$data/caddy/Caddyfile"
	else
		# shellcheck disable=SC2016 # a literal Caddy placeholder
		grep -v '^[[:space:]]*email {\$ACME_EMAIL}' "$stage/Caddyfile" >"$data/caddy/Caddyfile"
		chmod 644 "$data/caddy/Caddyfile"
	fi
	if [[ -n "$legacy" && -d "$legacy/sites" ]]; then
		for f in "$legacy"/sites/*.caddy; do
			[[ -f "$f" && ! -e "$data/caddy/sites/${f##*/}" ]] && install -m 0644 "$f" "$data/caddy/sites/"
		done
	fi
	if [[ -n "$registry" ]]; then
		install -m 0644 "$stage/registry.caddy" "$data/caddy/sites/registry.caddy"
	else
		rm -f "$data/caddy/sites/registry.caddy"
	fi
fi

# ---- Installer settings (.env) ----------------------------------------------------
env_file=$data/.env
if [[ ! -f "$env_file" ]]; then
	cat >"$env_file" <<'EOF'
# Settings of this installation, written by install.sh. No secrets: those
# are files in secrets/. Re-run the installer to change a setting rather than
# editing it here. Resource limits read by compose.yaml (HANGAR_MEMORY,
# POSTGRES_SHARED_BUFFERS, ...) may be added below; the installer keeps them.
EOF
	if [[ -n "$legacy" ]]; then
		for k in HANGAR_CPUS HANGAR_MEMORY POSTGRES_IMAGE POSTGRES_CPUS POSTGRES_MEMORY POSTGRES_MAX_CONNECTIONS \
			POSTGRES_SHARED_BUFFERS POSTGRES_EFFECTIVE_CACHE_SIZE POSTGRES_WORK_MEM POSTGRES_MAINTENANCE_WORK_MEM \
			CADDY_IMAGE CADDY_CPUS CADDY_MEMORY; do
			value=$(stored "$k" || true)
			[[ -z "$value" ]] || printf '%s=%s\n' "$k" "$value" >>"$env_file"
		done
	fi
fi
trusted_all=$subnet
[[ -n "$trusted" ]] && trusted_all=$trusted,$subnet
profiles=""
[[ "$tls" == caddy ]] && profiles=caddy
env_set "$env_file" HANGAR_VERSION "$version"
env_set "$env_file" HANGAR_IMAGE "$image"
env_set "$env_file" HANGAR_DOMAIN "$domain"
env_set "$env_file" HANGAR_TLS "$tls"
env_set "$env_file" HANGAR_BASE_URL "$base_url"
env_set "$env_file" HANGAR_SESSION_INSECURE "$insecure"
env_set "$env_file" HANGAR_BIND_ADDR "$bind"
env_set "$env_file" HANGAR_HTTP_PORT "$port"
env_set "$env_file" HANGAR_NETWORK_SUBNET "$subnet"
env_set "$env_file" HANGAR_INSTALL_TRUSTED_PROXIES "$trusted"
env_set "$env_file" HANGAR_TRUSTED_PROXIES "$trusted_all"
env_set "$env_file" HANGAR_REGISTRY_DOMAIN "$registry"
env_set "$env_file" ACME_EMAIL "$acme"
env_set "$env_file" COMPOSE_PROFILES "$profiles"
env_set "$env_file" POSTGRES_USER "$pg_user"
env_set "$env_file" POSTGRES_DB "$pg_db"

# ---- Images -----------------------------------------------------------------------
compose config --quiet || die "the compose configuration is invalid (see above)"
log "pulling images"
if ! compose pull --quiet; then
	# Offline hosts: images loaded with `docker load` are fine.
	while IFS= read -r img; do
		docker image inspect "$img" >/dev/null 2>&1 || die "cannot pull $img and it is not present locally"
	done < <(compose config --images)
	warn "could not pull every image; using the local copies"
fi

# Before an upgrade to another version: a database dump to fall back on, as
# migrations run on start. The three newest are kept.
if ((installed)) && [[ "$previous_version" != "$version" ]] && [[ -n "$(compose ps --status running -q postgres 2>/dev/null)" ]]; then
	dump=$data/backups/pre-upgrade-${previous_version:-unknown}-$(date -u +%Y%m%dT%H%M%SZ).dump
	log "backing up the database to $dump"
	(umask 077 && compose exec -T postgres pg_dump -U "$pg_user" -d "$pg_db" --format=custom </dev/null >"$dump") ||
		die "database backup failed; nothing was changed (remove $dump and retry)"
	# shellcheck disable=SC2012
	ls -1t "$data"/backups/pre-upgrade-*.dump | tail -n +4 | while IFS= read -r old; do rm -f -- "$old"; done
fi

# ---- Migration: stop the old stack, copy its volumes ------------------------------
# copy_volume VOLUME SUBDIR DEST: copies VOLUME/SUBDIR into DEST, from the
# host when the volume is a local directory, else through a container.
copy_volume() {
	local mp src
	mp=$(docker volume inspect -f '{{ .Mountpoint }}' "$1")
	if [[ -d "$mp" ]]; then
		src=$mp${2:+/$2}
		if [[ -d "$src" ]]; then cp -a "$src/." "$3/"; fi
	else
		docker run --rm --network none --entrypoint sh -v "$1:/from:ro" -v "$3:/to:Z" "$(compose config --images | awk '/postgres/ && !p { print; p = 1 }')" \
			-c "if [ -d /from/$2 ]; then cp -a /from/$2/. /to/; fi"
	fi
}

if [[ -n "$legacy" ]]; then
	need=0
	for v in "$vol_pg" "$vol_data"; do
		mp=$(docker volume inspect -f '{{ .Mountpoint }}' "$v")
		[[ -d "$mp" ]] && need=$((need + $(du -sk "$mp" | awk '{ print $1 }')))
	done
	avail=$(df -Pk "$data" | awk 'NR == 2 { print $4 }')
	((need < avail)) || die "$data has ${avail} KiB free; the old volumes need ${need} KiB"
	migrating=1
	log "stopping the old installation in $legacy"
	(cd "$legacy" && docker compose down --remove-orphans)
	log "copying the database (volume $vol_pg)"
	copy_volume "$vol_pg" "" "$data/postgres"
	log "copying packages and job state (volume $vol_data)"
	copy_volume "$vol_data" blobs "$data/blobs"
	copy_volume "$vol_data" nats "$data/nats"
	chown -R "$POSTGRES_UID:$POSTGRES_UID" "$data/postgres"
	chmod 700 "$data/postgres"
	chown -R "$HANGAR_UID:$HANGAR_UID" "$data/blobs" "$data/nats"
fi

# ---- Start ------------------------------------------------------------------------
log "starting"
compose up -d --remove-orphans

probe_host=$bind
[[ "$probe_host" == 0.0.0.0 || "$probe_host" == "::" ]] && probe_host=127.0.0.1
[[ "$probe_host" == *:* ]] && probe_host="[$probe_host]"
log "waiting for http://$probe_host:$port/readyz (up to ${READY_TIMEOUT}s)"
deadline=$((SECONDS + READY_TIMEOUT))
until curl -fsS -o /dev/null --max-time 5 "http://$probe_host:$port/readyz" 2>/dev/null; do
	if ((SECONDS >= deadline)); then
		compose ps || true
		compose logs --tail 40 hangar || true
		die "Hangar did not become ready within ${READY_TIMEOUT}s (logs above)"
	fi
	sleep 3
done
running_version=$(curl -fsS --max-time 5 "http://$probe_host:$port/api/v1/system/status" 2>/dev/null |
	sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -n 1) || true

if [[ -n "$legacy" ]]; then
	migrating=0
	printf '%s %s\n' "$legacy" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$data/.migrated-from"
	rm -f "$data/.migrating"
fi
[[ -n "$(env_get "$env_file" HANGAR_INSTALLED)" ]] || env_set "$env_file" HANGAR_INSTALLED "$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# On the first start of an empty database the server creates "admin" with a
# one-time password and writes it to secrets/initial-admin-password (mode
# 600); it deletes the file after the first password change.
pw_file=$data/secrets/initial-admin-password
if ((fresh)); then
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		[[ -s "$pw_file" ]] && break
		sleep 1
	done
fi

# ---- Summary ----------------------------------------------------------------------
self="curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | sudo bash -s --"
echo
if ((fresh)); then
	echo "Hangar ${running_version:-$version} is running."
elif [[ -n "$legacy" ]]; then
	echo "Hangar ${running_version:-$version} runs from $data, moved from $legacy."
else
	echo "Hangar is upgraded to ${running_version:-$version}${previous_version:+ (was $previous_version)}."
fi
cat <<EOF

  URL:        $base_url
EOF
if ((fresh)) && [[ -s "$pw_file" ]]; then
	pw=$(<"$pw_file")
	pw=${pw//[[:space:]]/}
	cat <<EOF
  Username:   admin
  Password:   $pw
              Shown once. Sign in and choose your own password.
EOF
elif ((fresh)); then
	cat <<EOF
  Username:   admin (no one-time password: the database already had an
              administrator, or the password was changed. Set a new one with
              cd $data && docker compose exec hangar hangar admin reset-password --user admin)
EOF
elif [[ -n "$legacy" ]]; then
	echo "  Admin:      existing administrator kept (sign in as before)"
fi
cat <<EOF
  Data:       $data

  Logs:       cd $data && docker compose logs -f hangar
  Upgrade:    $self --data-dir $data
  Uninstall:  $self --data-dir $data --uninstall    (keeps $data; add --purge to delete it)
  Back up:    $data (secrets/ decrypts the credentials stored in the database)
EOF
if [[ "$tls" == proxy ]]; then
	echo
	echo "Point your reverse proxy at http://$(host_ip):$port (large uploads: docs/deploy/reverse-proxy.md)."
	[[ -n "$trusted" ]] ||
		echo "Re-run with --trusted-proxies <proxy address> so Hangar sees client addresses."
elif [[ "$tls" == caddy ]]; then
	echo
	echo "Caddy requests the certificate for $domain on first use: DNS must point here and ports 80/443 be open."
fi
if [[ -n "$legacy" ]]; then
	cat <<EOF

The old installation is kept unchanged for rollback: $legacy and the volumes
$vol_pg and $vol_data. To return to it (changes made since are lost):

  cd $data && docker compose down
  cd $legacy && docker compose up -d

When you no longer need it:

  docker volume rm $vol_pg $vol_data && rm -rf $legacy
EOF
fi

exit 0
}
