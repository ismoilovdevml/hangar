#!/usr/bin/env bash
# Tests for deploy/install.sh without Docker, root or network: fake docker,
# curl, chown, id, ip and sleep commands record what the installer runs, and
# the tests check the commands, files, modes and compose settings it leaves.
#
#   bash deploy/tests/install_test.sh            # every test
#   bash deploy/tests/install_test.sh migration  # tests whose name matches

set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
INSTALL=$ROOT/deploy/install.sh
COMPOSE_SRC=$ROOT/deploy/compose
FILTER=${1:-}

pass=0 fail=0 failed=()

# ---- Fixtures -----------------------------------------------------------------------

# setup: a fresh sandbox in $T with fake commands first on PATH.
setup() {
	T=$(mktemp -d)
	mkdir -p "$T/bin" "$T/log" "$T/releases" "$T/volumes"
	: >"$T/log/docker.log"
	: >"$T/log/chown.log"
	: >"$T/log/curl.log"

	cat >"$T/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s|%s\n' "$PWD" "$*" >>"$FAKE_LOG/docker.log"
case "$1" in
compose)
	shift
	case "$*" in
	"version --short") echo "${FAKE_COMPOSE_VERSION:-2.29.1}" ;;
	"config --quiet") [[ -f compose.yaml && -f .env ]] || exit 1 ;;
	"config --images")
		awk -F= '$1 == "HANGAR_IMAGE" { print $2 }' .env
		echo postgres:16.15-alpine ;;
	"ps --status running -q postgres") [[ -z "${FAKE_PG_RUNNING:-}" ]] || echo 0123abcd ;;
	exec\ -T\ postgres\ pg_dump*)
		# Like the real exec, read stdin: under `curl | bash` that is the script.
		cat >/dev/null
		echo "PGDMP fake dump" ;;
	pull*) [[ -z "${FAKE_OFFLINE:-}" ]] ;;
	"up -d --remove-orphans")
		# The server's first start on an empty database: it creates "admin"
		# and writes a one-time password (bootstrap.initial_password_file).
		pw=secrets/initial-admin-password
		[[ ! -e "$pw" ]] || echo "password file existed before the first start" >>"$FAKE_LOG/docker.log"
		if [[ -z "$(ls -A postgres 2>/dev/null)" && -z "${FAKE_NO_PASSWORD:-}" ]]; then
			mkdir -p postgres/base && : >postgres/PG_VERSION
			(umask 077 && echo "Srv0Generated0Password24" >"$pw")
		fi ;;
	*) : ;;
	esac ;;
info) : ;;
inspect) [[ -n "${FAKE_RUNNING_DIR:-}" ]] || exit 1; echo "$FAKE_RUNNING_DIR" ;;
volume)
	name=${*: -1}
	[[ -d "$FAKE_VOLUMES/$name" ]] || exit 1
	[[ "$*" != *Mountpoint* ]] || echo "$FAKE_VOLUMES/$name" ;;
image) [[ -z "${FAKE_NO_LOCAL_IMAGES:-}" ]] ;;
*) : ;;
esac
EOF

	cat >"$T/bin/curl" <<'EOF'
#!/usr/bin/env bash
out="" url=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	--retry | --connect-timeout | --max-time) shift 2 ;;
	http*) url=$1; shift ;;
	*) shift ;;
	esac
done
echo "$url" >>"$FAKE_LOG/curl.log"
emit() { if [[ -n "$out" ]]; then cp "$1" "$out"; else cat "$1"; fi; }
case "$url" in
*/readyz) [[ -z "${FAKE_NOT_READY:-}" ]] ;;
*/api/v1/system/status) echo '{"version":"9.9.9","nodes":[]}' ;;
https://api.github.com/repos/ismoilovdevml/hangar/releases/latest)
	printf '{\n  "tag_name": "%s",\n  "name": "x"\n}\n' "${FAKE_LATEST:-v9.9.9}" ;;
https://github.com/ismoilovdevml/hangar/releases/download/*)
	f=$FAKE_RELEASES/${url#https://github.com/ismoilovdevml/hangar/releases/download/}
	[[ -f "$f" ]] || exit 22
	emit "$f" ;;
"$FAKE_MIRROR"/*)
	f=$FAKE_RELEASES/${url#"$FAKE_MIRROR"/}
	[[ -f "$f" ]] || exit 22
	emit "$f" ;;
*) exit 6 ;;
esac
EOF

	cat >"$T/bin/chown" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_LOG/chown.log"
EOF
	printf '#!/usr/bin/env bash\necho 0\n' >"$T/bin/id"
	printf '#!/usr/bin/env bash\n:\n' >"$T/bin/sleep"
	printf '#!/usr/bin/env bash\necho "1.1.1.1 via 10.0.0.1 dev eth0 src 10.0.0.5 uid 0"\n' >"$T/bin/ip"
	chmod +x "$T/bin/"*

	make_release v9.9.9
	export PATH="$T/bin:$PATH" FAKE_LOG=$T/log FAKE_RELEASES=$T/releases FAKE_VOLUMES=$T/volumes
	export FAKE_MIRROR=http://mirror.example.com/hangar
	export HANGAR_INSTALL_TTY=$T/no-terminal HANGAR_INSTALL_READY_TIMEOUT=2
	unset FAKE_RUNNING_DIR FAKE_PG_RUNNING FAKE_OFFLINE FAKE_NOT_READY FAKE_NO_LOCAL_IMAGES FAKE_LATEST FAKE_NO_PASSWORD
}

teardown() { rm -rf "$T"; }

# make_release VERSION: a release like the release workflow publishes it.
make_release() {
	local d=$T/releases/$1 s
	s=$(mktemp -d)
	mkdir -p "$d"
	cp "$COMPOSE_SRC"/{compose.yaml,hangar.yaml,Caddyfile,compose.connectors.example.yaml} "$s/"
	cp "$COMPOSE_SRC/sites/registry.caddy" "$s/"
	tar -C "$s" -czf "$d/hangar-compose.tar.gz" compose.yaml hangar.yaml Caddyfile registry.caddy compose.connectors.example.yaml
	cp "$INSTALL" "$d/install.sh"
	(cd "$d" && for f in hangar-compose.tar.gz install.sh; do printf '%s  %s\n' "$(sha256 "$f")" "$f"; done >SHA256SUMS)
	rm -rf "$s"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# A tree checksum: paths, modes and contents.
tree_sum() {
	(cd "$1" && find . -print0 | LC_ALL=C sort -z | while IFS= read -r -d '' f; do
		printf '%s %s ' "$f" "$(mode "$f")"
		if [[ -f "$f" ]]; then sha256 "$f"; else echo; fi
	done) | sha256 /dev/stdin
}

mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# An installation in the layout of earlier installers: /opt/hangar with .env,
# compose files and two named volumes.
make_legacy() {
	L=$T/opt-hangar
	mkdir -p "$L/sites"
	cat >"$L/.env" <<'EOF'
# Hangar compose stack settings.
COMPOSE_PROFILES=postgres
HANGAR_VERSION=v0.1.1
HANGAR_IMAGE=ghcr.io/ismoilovdevml/hangar:0.1.1
HANGAR_DOMAIN=hangar.example.com
HANGAR_REGISTRY_DOMAIN=
ACME_EMAIL=ops@example.com
HANGAR_BASE_URL=https://hangar.example.com
HANGAR_BIND_ADDR=0.0.0.0
HANGAR_HTTP_PORT=8090
HANGAR_NETWORK_SUBNET=172.30.80.0/24
HANGAR_INSTALL_TRUSTED_PROXIES=172.17.1.6/32
HANGAR_TRUSTED_PROXIES=172.17.1.6/32,172.30.80.0/24
HANGAR_BOOTSTRAP_ADMIN_USER=admin
HANGAR_BOOTSTRAP_ADMIN_PASSWORD=0123456789abcdef01234567
HANGAR_DOCKER_TOKEN_KEY=bGVnYWN5LWRvY2tlci10b2tlbi1rZXktbGVnYWN5LWRvY2tlci10b2tlbi1rZXk=
HANGAR_SECRET_KEY=bGVnYWN5LXNlY3JldC1rZXktMzItYnl0ZXMtbG9uZyE=
POSTGRES_USER=hangar
POSTGRES_PASSWORD=legacy0pg0password0hex000
POSTGRES_DB=hangar
HANGAR_STORAGE_KIND=fs
HANGAR_STORAGE_S3_BUCKET=hangar
MINIO_ROOT_USER=minio-root
HANGAR_AUTH_OIDC_CLIENT_SECRET="legacy-oidc-secret"
HANGAR_MEMORY=6g
EOF
	echo "name: hangar" >"$L/compose.yaml"
	cat >"$L/hangar.yaml" <<'EOF'
server:
  addr: ":8080"
auth:
  oidc:
    enabled: true
    issuer_url: https://sso.example.com/realms/acme
    client_id: hangar
EOF
	cp "$COMPOSE_SRC/Caddyfile" "$L/Caddyfile"
	mkdir -p "$T/volumes/hangar_postgres-data/base/1" "$T/volumes/hangar_hangar-data/blobs/sha256/ab" \
		"$T/volumes/hangar_hangar-data/nats/jetstream"
	echo 16 >"$T/volumes/hangar_postgres-data/PG_VERSION"
	echo "table" >"$T/volumes/hangar_postgres-data/base/1/16384"
	echo "blob" >"$T/volumes/hangar_hangar-data/blobs/sha256/ab/abcdef"
	echo "stream" >"$T/volumes/hangar_hangar-data/nats/jetstream/meta.inf"
}

run_install() {
	OUT=$T/out.txt
	bash "$INSTALL" "$@" </dev/null >"$OUT" 2>&1
	RC=$?
}

# ---- Assertions --------------------------------------------------------------------
fails=0
check() {
	local msg=$1
	shift
	if ! "$@"; then
		echo "    FAIL: $msg"
		fails=$((fails + 1))
	fi
}
contains() { grep -qF -- "$2" "$1"; }
absent() { ! grep -qF -- "$2" "$1"; }
env_is() { [[ "$(awk -F= -v k="$2" 'index($0, k "=") == 1 { print substr($0, length(k) + 2) }' "$1/.env")" == "$3" ]]; }
mode_is() { [[ "$(mode "$1")" == "$2" ]]; }
ok() { [[ $RC -eq 0 ]] || { sed 's/^/      | /' "$OUT"; false; }; }
failed_with() { [[ $RC -ne 0 ]] && grep -qF -- "$1" "$OUT"; }
matches() { [[ "$1" =~ $2 ]]; }
# The Caddyfile's e-mail line, a literal Caddy placeholder.
EMAIL_LINE="email {\$ACME_EMAIL}"
count_is() { [[ "$(grep -cF -- "$2" "$1")" == "$3" ]]; }
# docker_order A B: docker command A (substring) is logged before B.
docker_order() {
	local a b
	a=$(grep -nF -- "$1" "$T/log/docker.log" | head -n1 | cut -d: -f1)
	b=$(grep -nF -- "$2" "$T/log/docker.log" | head -n1 | cut -d: -f1)
	[[ -n "$a" && -n "$b" && $a -lt $b ]]
}

# ---- Tests -------------------------------------------------------------------------

test_fresh_install_unattended() {
	local D=$T/srv/hangar
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "exit 0" ok
	for d in config secrets blobs nats postgres backups; do check "$d/ exists" test -d "$D/$d"; done
	check "secrets/ is 700" mode_is "$D/secrets" 700
	check "postgres/ is 700" mode_is "$D/postgres" 700
	check "blobs/ is 750" mode_is "$D/blobs" 750
	check "no caddy/ without TLS" test ! -e "$D/caddy"
	for f in security.secret_key docker.token_key database.url postgres-password; do
		check "secrets/$f is 600" mode_is "$D/secrets/$f" 600
	done
	check "secrets/ owned by the hangar user" contains "$T/log/chown.log" "65532:65532 $D/secrets"
	check "blobs/ owned by the hangar user" contains "$T/log/chown.log" "65532:65532 $D/blobs"
	check "nats/ owned by the hangar user" contains "$T/log/chown.log" "65532:65532 $D/nats"
	check "postgres/ owned by the postgres user" contains "$T/log/chown.log" "70:70 $D/postgres"
	check "postgres-password owned by the postgres user" contains "$T/log/chown.log" "70:70 $D/secrets/.postgres-password.tmp"
	check "secret key is 32 bytes" test "$(base64 -d <"$D/secrets/security.secret_key" | wc -c | tr -d ' ')" = 32
	check "token key is 48 bytes" test "$(base64 -d <"$D/secrets/docker.token_key" | wc -c | tr -d ' ')" = 48
	local pg pw
	pg=$(<"$D/secrets/postgres-password")
	pw=$(<"$D/secrets/initial-admin-password")
	check "database.url carries the postgres password" test "$(<"$D/secrets/database.url")" = "postgres://hangar:$pg@postgres:5432/hangar?sslmode=disable"
	check "password file left to the server" absent "$T/log/docker.log" "password file existed"
	check "server-generated password read" test "$pw" = Srv0Generated0Password24
	check "admin password printed once" count_is "$OUT" "$pw" 1
	check "summary names the user" contains "$OUT" "Username:   admin"
	check "summary has the URL" contains "$OUT" "URL:        http://10.0.0.5:8080"
	check "summary explains upgrades" contains "$OUT" "--data-dir $D"
	check "base URL from the host address" env_is "$D" HANGAR_BASE_URL http://10.0.0.5:8080
	check "insecure cookies for plain HTTP" env_is "$D" HANGAR_SESSION_INSECURE true
	check "published on every address" env_is "$D" HANGAR_BIND_ADDR 0.0.0.0
	check "release image" env_is "$D" HANGAR_IMAGE ghcr.io/ismoilovdevml/hangar:9.9.9
	check "no compose profiles" env_is "$D" COMPOSE_PROFILES ""
	check "install recorded" grep -q '^HANGAR_INSTALLED=20' "$D/.env"
	for f in security.secret_key docker.token_key postgres-password; do
		check ".env has no secret ($f)" absent "$D/.env" "$(<"$D/secrets/$f")"
	done
	check "compose.yaml is the release file" cmp -s "$D/compose.yaml" "$COMPOSE_SRC/compose.yaml"
	check "hangar.yaml installed" cmp -s "$D/config/hangar.yaml" "$COMPOSE_SRC/hangar.yaml"
	check "checksum verified download" contains "$T/log/curl.log" "releases/download/v9.9.9/SHA256SUMS"
	check "started in the data directory" contains "$T/log/docker.log" "$D|compose up -d --remove-orphans"
	check "pulled" contains "$T/log/docker.log" "$D|compose pull --quiet"
	check "no Docker volumes used" absent "$T/log/docker.log" "volume"
	check "no backup on first install" absent "$T/log/docker.log" "pg_dump"
	check "readiness probed" contains "$T/log/curl.log" "http://127.0.0.1:8080/readyz"
}

test_rerun_upgrades_in_place() {
	local D=$T/srv/hangar
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "first run" ok
	local before
	before=$(tree_sum "$D/secrets")
	echo "# my tuning" >>"$D/config/hangar.yaml"
	cp "$D/config/hangar.yaml" "$T/hangar.yaml.mine"
	echo "# local edit" >>"$D/compose.yaml"
	echo "HANGAR_MEMORY=8g" >>"$D/.env"
	make_release v9.9.10
	: >"$T/log/docker.log"
	export FAKE_PG_RUNNING=1
	run_install --data-dir "$D" --version v9.9.10 --yes
	check "exit 0" ok
	check "secrets unchanged" test "$(tree_sum "$D/secrets")" = "$before"
	check "hangar.yaml kept" cmp -s "$D/config/hangar.yaml" "$T/hangar.yaml.mine"
	check "compose.yaml replaced" cmp -s "$D/compose.yaml" "$COMPOSE_SRC/compose.yaml"
	check "version updated" env_is "$D" HANGAR_VERSION v9.9.10
	check "image updated" env_is "$D" HANGAR_IMAGE ghcr.io/ismoilovdevml/hangar:9.9.10
	check "extra .env keys kept" env_is "$D" HANGAR_MEMORY 8g
	check "settings kept" env_is "$D" HANGAR_BASE_URL http://10.0.0.5:8080
	check "database dumped before the upgrade" contains "$T/log/docker.log" "$D|compose exec -T postgres pg_dump -U hangar -d hangar --format=custom"
	check "dump before start" docker_order "pg_dump" "compose up -d"
	local dump
	dump=$(find "$D/backups" -name 'pre-upgrade-v9.9.9-*.dump' | head -n1)
	check "dump file written" test -s "$dump"
	check "dump is private" mode_is "$dump" 600
	check "no password on upgrade" absent "$OUT" "Password:"
	check "upgrade message" contains "$OUT" "upgraded to 9.9.9 (was v9.9.9)"

	# Same version again: no new dump.
	: >"$T/log/docker.log"
	run_install --data-dir "$D" --version v9.9.10 --yes
	check "rerun same version" ok
	check "no dump for the same version" absent "$T/log/docker.log" "pg_dump"
	check "latest by default" true
}

# The documented `curl ... | sudo bash` form: the script arrives on stdin.
test_piped_script() {
	local D=$T/srv/hangar
	bash -s -- --data-dir "$D" --version v9.9.9 --yes <"$INSTALL" >"$T/out.txt" 2>&1
	RC=$? OUT=$T/out.txt
	check "fresh install from stdin" ok
	export FAKE_PG_RUNNING=1
	make_release v9.9.10
	bash -s -- --data-dir "$D" --version v9.9.10 --yes <"$INSTALL" >"$OUT" 2>&1
	RC=$?
	check "upgrade from stdin (exec reads stdin)" ok
	check "finished after the dump" contains "$OUT" "upgraded to"
	# A truncated download runs nothing.
	head -c 4000 "$INSTALL" >"$T/partial.sh"
	: >"$T/log/docker.log"
	bash -s -- --data-dir "$T/other" --yes <"$T/partial.sh" >"$OUT" 2>&1
	RC=$?
	check "truncated script fails" test "$RC" -ne 0
	check "truncated script runs no docker command" test ! -s "$T/log/docker.log"
}

test_latest_version_lookup() {
	make_release v9.9.11
	export FAKE_LATEST=v9.9.11
	run_install --data-dir "$T/d" --yes
	check "exit 0" ok
	check "latest asked from the API" contains "$T/log/curl.log" "api.github.com/repos/ismoilovdevml/hangar/releases/latest"
	check "latest image" env_is "$T/d" HANGAR_IMAGE ghcr.io/ismoilovdevml/hangar:9.9.11
}

test_interactive_domain_with_caddy() {
	local D=$T/data
	printf '%s\n' "$D" hangar.example.com 1 "" y >"$T/answers"
	export HANGAR_INSTALL_TTY=$T/answers
	run_install --version v9.9.9
	check "exit 0" ok
	check "asked for the data directory" contains "$OUT" "Data directory [/srv/hangar]:"
	check "asked for the domain" contains "$OUT" "Domain name (empty: http://10.0.0.5:8080):"
	check "asked about TLS" contains "$OUT" "How does hangar.example.com get HTTPS?"
	check "tls caddy" env_is "$D" HANGAR_TLS caddy
	check "caddy profile" env_is "$D" COMPOSE_PROFILES caddy
	check "asked about image scanning" contains "$OUT" "Scan Docker images for vulnerabilities with Trivy"
	check "no image scanning by default" env_is "$D" HANGAR_TRIVY off
	check "scanner.env without settings" absent "$D/scanner.env" "HANGAR_SCANNER_TRIVY_URL"
	check "loopback behind caddy" env_is "$D" HANGAR_BIND_ADDR 127.0.0.1
	check "https base URL" env_is "$D" HANGAR_BASE_URL https://hangar.example.com
	check "secure cookies" env_is "$D" HANGAR_SESSION_INSECURE false
	for d in caddy caddy/sites caddy/data caddy/config; do check "$d exists" test -d "$D/$d"; done
	check "Caddyfile written" test -f "$D/caddy/Caddyfile"
	check "no e-mail line without --acme-email" absent "$D/caddy/Caddyfile" "$EMAIL_LINE"
	check "no registry site" test ! -e "$D/caddy/sites/registry.caddy"
	check "certificate note" contains "$OUT" "Caddy requests the certificate for hangar.example.com"
}

test_interactive_domain_behind_proxy() {
	local D=$T/data
	printf '%s\n' "$D" hangar.example.com 2 y y >"$T/answers"
	export HANGAR_INSTALL_TTY=$T/answers
	run_install --version v9.9.9 --port 8090
	check "exit 0" ok
	check "tls proxy" env_is "$D" HANGAR_TLS proxy
	check "port" env_is "$D" HANGAR_HTTP_PORT 8090
	check "reachable by a remote proxy" env_is "$D" HANGAR_BIND_ADDR 0.0.0.0
	check "https base URL" env_is "$D" HANGAR_BASE_URL https://hangar.example.com
	check "trivy profile when answered yes" env_is "$D" COMPOSE_PROFILES trivy
	check "no caddy/" test ! -e "$D/caddy"
	check "trusted proxies hint" contains "$OUT" "--trusted-proxies"
}

test_interactive_cancel() {
	printf '%s\n' "$T/data" "" "" n >"$T/answers"
	export HANGAR_INSTALL_TTY=$T/answers
	run_install --version v9.9.9
	check "cancelled" failed_with "cancelled"
	check "nothing written" test ! -e "$T/data"
	check "nothing started" absent "$T/log/docker.log" "compose up"
}

test_unattended_flags() {
	local D=$T/d
	run_install --data-dir "$D" --domain hangar.example.com --tls caddy --acme-email ops@corp.example \
		--registry-domain registry.example.org --trusted-proxies 10.0.0.5/32 --with-trivy --version v9.9.9 --yes
	check "exit 0" ok
	check "e-mail kept in Caddyfile" contains "$D/caddy/Caddyfile" "$EMAIL_LINE"
	check "acme e-mail" env_is "$D" ACME_EMAIL ops@corp.example
	check "registry site" cmp -s "$D/caddy/sites/registry.caddy" "$COMPOSE_SRC/sites/registry.caddy"
	check "registry domain" env_is "$D" HANGAR_REGISTRY_DOMAIN registry.example.org
	check "trusted proxies plus the stack network" env_is "$D" HANGAR_TRUSTED_PROXIES 10.0.0.5/32,172.30.80.0/24
	# Every bind mount source of compose.yaml exists (create_host_path is off).
	local src
	while read -r src; do
		check "bind source $src exists" test -e "$D/$src"
	done < <(sed -n 's|^ *source: \./||p' "$COMPOSE_SRC/compose.yaml")
	check "bind sources found" test "$(grep -c 'source: \./' "$COMPOSE_SRC/compose.yaml")" -ge 10

	# --tls none is the old name of proxy.
	run_install --data-dir "$T/e" --domain hangar.example.com --tls none --version v9.9.9 --yes
	check "tls none" ok
	check "none means proxy" env_is "$T/e" HANGAR_TLS proxy
}

test_unattended_needs_data_dir() {
	run_install --domain hangar.example.com --version v9.9.9
	check "refused" failed_with "--data-dir is required without a terminal"
	run_install --data-dir relative/path --version v9.9.9 --yes
	check "relative path refused" failed_with "absolute path"
	mkdir -p "$T/busy" && touch "$T/busy/file"
	run_install --data-dir "$T/busy" --version v9.9.9 --yes
	check "non-empty directory refused" failed_with "not empty"
	run_install --data-dir "$T/x" --tls caddy --version v9.9.9 --yes
	check "tls without domain refused" failed_with "--tls needs --domain"
	run_install --dir /opt/hangar
	check "--dir explains the new flags" failed_with "--migrate-from"
	check "nothing started" absent "$T/log/docker.log" "compose up"
}

test_preconditions() {
	export FAKE_COMPOSE_VERSION=2.12.0
	run_install --data-dir "$T/d" --version v9.9.9 --yes
	check "old compose refused" failed_with "2.20 or newer"
	unset FAKE_COMPOSE_VERSION
	printf '#!/usr/bin/env bash\necho 1000\n' >"$T/bin/id"
	run_install --data-dir "$T/d" --version v9.9.9 --yes
	check "root required" failed_with "run as root"
}

test_release_checks() {
	# Tampered archive.
	echo "tampered" >>"$T/releases/v9.9.9/hangar-compose.tar.gz"
	run_install --data-dir "$T/d" --version v9.9.9 --yes
	check "checksum mismatch refused" failed_with "checksum mismatch"
	check "nothing written after a bad download" test ! -e "$T/d"
	# A release from before the compose archive.
	mkdir -p "$T/releases/v0.1.1"
	echo "abc  hangar-linux-amd64" >"$T/releases/v0.1.1/SHA256SUMS"
	run_install --data-dir "$T/d" --version v0.1.1 --yes
	check "old release refused" failed_with "predates this installer"
	# A compose archive in the old layout (secrets in .env).
	local s=$T/old-stack
	mkdir -p "$s" "$T/releases/v0.1.4"
	echo "services: {}" >"$s/compose.yaml"
	for f in hangar.yaml Caddyfile registry.caddy compose.connectors.example.yaml; do echo "#" >"$s/$f"; done
	tar -C "$s" -czf "$T/releases/v0.1.4/hangar-compose.tar.gz" compose.yaml hangar.yaml Caddyfile registry.caddy compose.connectors.example.yaml
	printf '%s  hangar-compose.tar.gz\n' "$(sha256 "$T/releases/v0.1.4/hangar-compose.tar.gz")" >"$T/releases/v0.1.4/SHA256SUMS"
	run_install --data-dir "$T/d" --version v0.1.4 --yes
	check "old layout refused" failed_with "uses the old layout"
}

test_offline_sources() {
	run_install --data-dir "$T/a" --mirror "$FAKE_MIRROR" --version v9.9.9 --yes
	check "mirror" ok
	check "mirror used" contains "$T/log/curl.log" "$FAKE_MIRROR/v9.9.9/hangar-compose.tar.gz"
	run_install --data-dir "$T/b" --mirror "$FAKE_MIRROR" --yes
	check "mirror needs a version" failed_with "--mirror needs --version"

	: >"$T/log/curl.log"
	run_install --data-dir "$T/c" --files-from "$COMPOSE_SRC" --image registry.example.org/hangar:dev --yes
	check "files from a directory" ok
	check "no download" absent "$T/log/curl.log" "github.com"
	check "custom image" env_is "$T/c" HANGAR_IMAGE registry.example.org/hangar:dev
	check "local version" env_is "$T/c" HANGAR_VERSION local

	run_install --data-dir "$T/e" --files-from "$T/releases/v9.9.9/hangar-compose.tar.gz" --version v9.9.9 --yes
	check "files from the archive" ok
	check "archive compose file" cmp -s "$T/e/compose.yaml" "$COMPOSE_SRC/compose.yaml"

	export FAKE_OFFLINE=1
	run_install --data-dir "$T/f" --version v9.9.9 --yes
	check "local images when pulls fail" ok
	check "offline warning" contains "$OUT" "using the local copies"
	export FAKE_NO_LOCAL_IMAGES=1
	run_install --data-dir "$T/g" --version v9.9.9 --yes
	check "missing images refused" failed_with "not present locally"
}

test_migration_from_old_layout() {
	make_legacy
	local D=$T/srv/hangar legacy_before vol_before
	legacy_before=$(tree_sum "$L")
	vol_before=$(tree_sum "$T/volumes")
	export FAKE_RUNNING_DIR=$L
	run_install --migrate-from "$L" --data-dir "$D" --version v9.9.9 --yes
	check "exit 0" ok
	# Secrets out of .env, into files.
	check "secret key" test "$(<"$D/secrets/security.secret_key")" = "bGVnYWN5LXNlY3JldC1rZXktMzItYnl0ZXMtbG9uZyE="
	check "token key" test "$(<"$D/secrets/docker.token_key")" = "bGVnYWN5LWRvY2tlci10b2tlbi1rZXktbGVnYWN5LWRvY2tlci10b2tlbi1rZXk="
	check "oidc secret (quotes removed)" test "$(<"$D/secrets/auth.oidc.client_secret")" = "legacy-oidc-secret"
	check "postgres password" test "$(<"$D/secrets/postgres-password")" = "legacy0pg0password0hex000"
	check "database url" test "$(<"$D/secrets/database.url")" = "postgres://hangar:legacy0pg0password0hex000@postgres:5432/hangar?sslmode=disable"
	check "no initial password: the admin exists" test ! -e "$D/secrets/initial-admin-password"
	check "existing administrator kept" contains "$OUT" "existing administrator kept"
	check "no password printed" absent "$OUT" "Password:"
	for f in "$D"/secrets/*; do check "${f##*/} is 600" mode_is "$f" 600; done
	# Settings carried over.
	check "port" env_is "$D" HANGAR_HTTP_PORT 8090
	check "bind" env_is "$D" HANGAR_BIND_ADDR 0.0.0.0
	check "trusted proxies" env_is "$D" HANGAR_INSTALL_TRUSTED_PROXIES 172.17.1.6/32
	check "trusted proxies with the network" env_is "$D" HANGAR_TRUSTED_PROXIES 172.17.1.6/32,172.30.80.0/24
	check "base URL" env_is "$D" HANGAR_BASE_URL https://hangar.example.com
	check "behind the proxy" env_is "$D" HANGAR_TLS proxy
	check "secure cookies" env_is "$D" HANGAR_SESSION_INSECURE false
	check "resource limit carried" env_is "$D" HANGAR_MEMORY 6g
	check "placeholder e-mail dropped" env_is "$D" ACME_EMAIL ""
	check "new image" env_is "$D" HANGAR_IMAGE ghcr.io/ismoilovdevml/hangar:9.9.9
	check "no secret left in .env" absent "$D/.env" "legacy0pg0password0hex000"
	check "hangar.yaml with the oidc block kept" cmp -s "$D/config/hangar.yaml" "$L/hangar.yaml"
	# Data copied.
	check "database copied" cmp -s "$D/postgres/base/1/16384" "$T/volumes/hangar_postgres-data/base/1/16384"
	check "PG_VERSION copied" test -f "$D/postgres/PG_VERSION"
	check "blobs copied" cmp -s "$D/blobs/sha256/ab/abcdef" "$T/volumes/hangar_hangar-data/blobs/sha256/ab/abcdef"
	check "nats copied" test -f "$D/nats/jetstream/meta.inf"
	check "postgres data owned by postgres" contains "$T/log/chown.log" "-R 70:70 $D/postgres"
	check "blobs owned by hangar" contains "$T/log/chown.log" "-R 65532:65532 $D/blobs $D/nats"
	# Order: images first, then stop the old stack, copy, start the new one.
	check "old stack stopped" contains "$T/log/docker.log" "$L|compose down --remove-orphans"
	check "pull before stop" docker_order "$D|compose pull" "$L|compose down"
	check "stop before start" docker_order "$L|compose down" "$D|compose up -d"
	check "old volumes never removed" absent "$T/log/docker.log" "volume rm"
	check "old directory unchanged" test "$(tree_sum "$L")" = "$legacy_before"
	check "old volumes unchanged" test "$(tree_sum "$T/volumes")" = "$vol_before"
	check "marked as migrated" test -f "$D/.migrated-from"
	check "no unfinished marker" test ! -e "$D/.migrating"
	check "rollback printed" contains "$OUT" "cd $L && docker compose up -d"
	check "cleanup printed" contains "$OUT" "docker volume rm hangar_postgres-data hangar_hangar-data"

	# Re-running on the migrated directory is an upgrade; the old one stays.
	: >"$T/log/docker.log"
	export FAKE_RUNNING_DIR=$D
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "rerun after migration" ok
	check "old stack untouched on rerun" absent "$T/log/docker.log" "$L|"
	check "settings kept on rerun" env_is "$D" HANGAR_HTTP_PORT 8090
}

test_migration_detected() {
	make_legacy
	export FAKE_RUNNING_DIR=$L
	run_install --data-dir "$T/srv" --version v9.9.9 --yes
	check "unattended: explicit flag required" failed_with "--migrate-from $L --data-dir $T/srv"
	check "nothing stopped" absent "$T/log/docker.log" "compose down"

	printf '%s\n' "$T/srv" y y >"$T/answers"
	export HANGAR_INSTALL_TTY=$T/answers
	run_install --version v9.9.9
	check "interactive: offered and done" ok
	check "offer asked" contains "$OUT" "Move it into $T/srv?"
	check "migrated" test -f "$T/srv/.migrated-from"

	# The old directory as the data directory.
	export HANGAR_INSTALL_TTY=$T/no-terminal
	run_install --data-dir "$L" --version v9.9.9 --yes
	check "old layout as data dir refused" failed_with "--migrate-from $L"
}

test_migration_refusals() {
	make_legacy
	run_install --migrate-from "$L" --data-dir "$L/new" --version v9.9.9 --yes
	check "inside the old directory refused" failed_with "outside"
	sed -i.bak 's/^HANGAR_STORAGE_KIND=fs/HANGAR_STORAGE_KIND=s3/' "$L/.env"
	run_install --migrate-from "$L" --data-dir "$T/srv" --version v9.9.9 --yes
	check "s3 refused" failed_with "only moves local storage"
	check "nothing stopped" absent "$T/log/docker.log" "compose down"
	mv "$L/.env.bak" "$L/.env"
	rm -rf "$T/srv" "$T/volumes/hangar_hangar-data"
	run_install --migrate-from "$L" --data-dir "$T/srv" --version v9.9.9 --yes
	check "missing volume refused" failed_with "hangar_hangar-data"
	check "not stopped when a volume is missing" absent "$T/log/docker.log" "compose down"
}

test_migration_failure_prints_rollback() {
	make_legacy
	export FAKE_NOT_READY=1
	run_install --migrate-from "$L" --data-dir "$T/srv" --version v9.9.9 --yes
	check "fails" failed_with "did not become ready"
	check "rollback printed" contains "$OUT" "cd $L && docker compose up -d"
	check "unfinished marker kept" test -e "$T/srv/.migrating"
	unset FAKE_NOT_READY
	run_install --data-dir "$T/srv" --version v9.9.9 --yes
	check "rerun refused until cleaned up" failed_with "did not finish"
}

test_uninstall_and_purge() {
	local D=$T/d
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "installed" ok
	run_install --uninstall --data-dir "$D" --yes
	check "uninstall" ok
	check "containers removed" contains "$T/log/docker.log" "$D|compose down --remove-orphans"
	check "data kept" test -f "$D/secrets/security.secret_key"

	run_install --uninstall --purge --data-dir "$D"
	check "purge needs confirmation" failed_with "needs --yes"
	check "still there" test -d "$D"

	printf 'wrong\n' >"$T/answers"
	HANGAR_INSTALL_TTY=$T/answers run_install --uninstall --purge --data-dir "$D"
	check "wrong confirmation refused" failed_with "not confirmed"
	check "still there after a wrong answer" test -d "$D"

	printf '%s\n' "$D" >"$T/answers"
	HANGAR_INSTALL_TTY=$T/answers run_install --uninstall --purge --data-dir "$D"
	check "purged" ok
	check "data deleted" test ! -e "$D"

	run_install --uninstall --data-dir "$T/nothing" --yes
	check "unknown directory refused" failed_with "no Hangar installation"
	mkdir -p "$T/other" && touch "$T/other/keep"
	run_install --uninstall --purge --data-dir "$T/other" --yes
	check "purge never deletes a foreign directory" test -f "$T/other/keep"
}

# A failed first run (Hangar never got ready) prints the password on the
# run that succeeds, and only then.
test_password_after_failed_first_run() {
	local D=$T/d
	export FAKE_NOT_READY=1
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "first run fails" failed_with "did not become ready"
	check "no password on failure" absent "$OUT" "Password:"
	unset FAKE_NOT_READY
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "second run" ok
	check "password printed now" contains "$OUT" "Password:   $(<"$D/secrets/initial-admin-password")"
	run_install --data-dir "$D" --version v9.9.9 --yes
	check "third run" ok
	check "not again" absent "$OUT" "Password:"
}

test_image_scanning() {
	local D=$T/d
	run_install --data-dir "$D" --domain hangar.example.com --tls caddy --with-trivy \
		--trivy-proxy http://proxy.corp.example:3128 --version v9.9.9 --yes
	check "exit 0" ok
	check "profiles" env_is "$D" COMPOSE_PROFILES caddy,trivy
	check "trivy recorded" env_is "$D" HANGAR_TRIVY on
	check "scanner image pinned to the release" env_is "$D" HANGAR_SCANNER_IMAGE ghcr.io/ismoilovdevml/hangar-scanner:9.9.9
	check "proxy for the database" env_is "$D" TRIVY_HTTPS_PROXY http://proxy.corp.example:3128
	for d in trivy scanner; do
		check "$d/ is 750" mode_is "$D/$d" 750
		check "$d/ owned by the hangar user" contains "$T/log/chown.log" "65532:65532 $D/$d"
	done
	check "adapter token is 600" mode_is "$D/secrets/scanner.adapter_token" 600
	check "adapter token is 64 hex" matches "$(<"$D/secrets/scanner.adapter_token")" '^[0-9a-f]{64}$'
	check ".env has no adapter token" absent "$D/.env" "$(<"$D/secrets/scanner.adapter_token")"
	check "scanner.env wires the Trivy server" contains "$D/scanner.env" "HANGAR_SCANNER_TRIVY_URL=http://trivy:4954"
	check "scanner.env wires the adapter" contains "$D/scanner.env" "HANGAR_SCANNER_ADAPTER_URL=http://hangar-scanner:4955"
	check "scanner.env wires the registry" contains "$D/scanner.env" "HANGAR_SCANNER_REGISTRY_URL=http://hangar:8080"
	check "plan mentions scanning" contains "$OUT" "Image scanning: Trivy (database through http://proxy.corp.example:3128)"

	# Upgrades keep it on; --without-trivy turns it off and keeps the data.
	local token
	token=$(<"$D/secrets/scanner.adapter_token")
	make_release v9.9.10
	run_install --data-dir "$D" --version v9.9.10 --yes
	check "upgrade ok" ok
	check "still on" env_is "$D" COMPOSE_PROFILES caddy,trivy
	check "scanner image follows the release" env_is "$D" HANGAR_SCANNER_IMAGE ghcr.io/ismoilovdevml/hangar-scanner:9.9.10
	check "token kept" test "$(<"$D/secrets/scanner.adapter_token")" = "$token"
	check "proxy kept" env_is "$D" TRIVY_HTTPS_PROXY http://proxy.corp.example:3128
	run_install --data-dir "$D" --version v9.9.10 --without-trivy --yes
	check "turned off" env_is "$D" COMPOSE_PROFILES caddy
	check "off recorded" env_is "$D" HANGAR_TRIVY off
	check "scanner.env emptied" absent "$D/scanner.env" "HANGAR_SCANNER_TRIVY_URL"
	check "database kept" test -d "$D/trivy"

	run_install --data-dir "$T/e" --with-trivy --trivy-proxy "not a url" --version v9.9.9 --yes
	check "bad proxy refused" failed_with "--trivy-proxy"
	run_install --data-dir "$T/f" --with-trivy --image hangar:dev --files-from "$COMPOSE_SRC" --yes
	check "local image needs a scanner image" failed_with "--scanner-image"
	run_install --data-dir "$T/f" --with-trivy --image hangar:dev --scanner-image hangar-scanner:dev --files-from "$COMPOSE_SRC" --yes
	check "local scanner image" env_is "$T/f" HANGAR_SCANNER_IMAGE hangar-scanner:dev
}

# The database already had an administrator: no one-time password, and the
# summary says how to set one.
test_no_password_generated() {
	export FAKE_NO_PASSWORD=1
	run_install --data-dir "$T/d" --version v9.9.9 --yes
	check "exit 0" ok
	check "no password line" absent "$OUT" "Password:"
	check "reset hint" contains "$OUT" "hangar admin reset-password --user admin"
}

# ---- Runner -------------------------------------------------------------------------
for t in $(declare -F | awk '{ print $3 }' | grep '^test_'); do
	[[ -z "$FILTER" || "$t" == *"$FILTER"* ]] || continue
	fails=0
	setup
	"$t"
	teardown
	if ((fails == 0)); then
		pass=$((pass + 1))
		echo "ok   $t"
	else
		fail=$((fail + 1))
		failed+=("$t")
		echo "FAIL $t"
	fi
done
echo
echo "$pass passed, $fail failed"
((fail == 0)) || {
	printf '  %s\n' "${failed[@]}"
	exit 1
}
