#!/usr/bin/env bash
# LDAP and Active Directory acceptance test (test/e2e/ldap_test.go).
#
# Starts an OpenLDAP server and a Samba Active Directory domain controller
# in Docker, seeds users, groups and a nested group, gives both a
# certificate from a throwaway CA (StartTLS and ldaps are verified against
# it), then runs the Go test against a Hangar server:
#
#   test/e2e/ldap/run.sh                 # builds and starts hangar serve (with a PostgreSQL container)
#   HANGAR_E2E_URL=http://127.0.0.1:8080 HANGAR_E2E_TOKEN=hgr_... test/e2e/ldap/run.sh
#
# Needs a Linux host with Docker (the domain controller runs --privileged),
# bash, openssl and, to start Hangar, Go. Re-running is safe: the
# containers are recreated each time. Extra arguments go to `go test`.
#
#   KEEP=1                keep the containers (and Hangar) running afterwards
#   LDAP_E2E_DIRECTORIES  "openldap ad" (default), or one of them
#   LDAP_E2E_WORKDIR      scratch directory (default ${TMPDIR:-/tmp}/hangar-ldap-e2e)
#
# With HANGAR_E2E_URL the server must reach the directories at the
# addresses below (127.0.0.1 ports), must not set auth.ldap in its
# configuration file, and needs security.secret_key (the bind password is
# stored sealed). The test replaces its LDAP settings: use a disposable
# server.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
work=${LDAP_E2E_WORKDIR:-${TMPDIR:-/tmp}/hangar-ldap-e2e}
directories=${LDAP_E2E_DIRECTORIES:-openldap ad}

openldap_image=osixia/openldap:1.5.0
samba_image=diegogslomp/samba-ad-dc:4.24.7
postgres_image=postgres:16.15-alpine

openldap_name=hangar-e2e-openldap
samba_name=hangar-e2e-samba
postgres_name=hangar-e2e-ldap-postgres

openldap_port=13389 openldap_ldaps_port=13636
samba_port=23389 samba_ldaps_port=23636
postgres_port=15432 hangar_port=18089

# Directory contents. Passwords meet the Active Directory complexity rules
# (three character classes, no part of the account or display name).
ol_domain=example.org ol_base=dc=example,dc=org
ol_admin_pw=Qv4-Lm8-Rt2k ol_readonly_pw=Hs6-Pw3-Zn7c
ol_member=ol-jdoe ol_member_pw=Kq7-Pz2-Lm9x ol_other=ol-bob ol_other_pw=Wt4-Rb8-Hn3y
ad_realm=SAMDOM.EXAMPLE.COM ad_domain=SAMDOM ad_base=DC=samdom,DC=example,DC=com
ad_admin_pw=Ex5-Mf3-Tj8w ad_svc=hangar-svc ad_svc_pw=Vc6-Ys1-Gd5u
ad_member=ad-jdoe ad_member_pw=Nf2-Xc7-Bq4v ad_other=ad-bob ad_other_pw=Jp9-Uk3-Sd6m

log() { printf '\n== %s\n' "$*" >&2; }
die() { printf 'run.sh: %s\n' "$*" >&2; exit 1; }
has() { case " $directories " in *" $1 "*) return 0 ;; esac; return 1; }

for tool in docker openssl; do
	command -v "$tool" >/dev/null || die "$tool is required"
done
docker info >/dev/null 2>&1 || die "docker is not usable (is the daemon running, may this user use it?)"

hangar_pid=""
cleanup() {
	status=$?
	if [ "${KEEP:-}" = 1 ]; then
		log "KEEP=1: leaving the containers${hangar_pid:+ and hangar (pid $hangar_pid)} running"
		exit "$status"
	fi
	[ -n "$hangar_pid" ] && kill "$hangar_pid" 2>/dev/null && wait "$hangar_pid" 2>/dev/null
	docker rm -f "$openldap_name" "$samba_name" "$postgres_name" >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT INT TERM

# wait_for tries a command every 2s for up to $1 seconds.
wait_for() {
	local limit=$1 what=$2
	shift 2
	local end=$((SECONDS + limit))
	until "$@" >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$end" ] || die "timed out after ${limit}s waiting for $what"
		sleep 2
	done
}

rm -rf "$work"
mkdir -p "$work/tls"

log "certificates (CA + server certificate for 127.0.0.1 and localhost)"
(
	cd "$work/tls"
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=Hangar LDAP e2e CA" \
		-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
		-keyout ca.key -out ca.pem 2>/dev/null
	openssl req -newkey rsa:2048 -nodes -subj "/CN=localhost" -keyout key.pem -out server.csr 2>/dev/null
	printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n' >server.ext
	openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -extfile server.ext -out cert.pem 2>/dev/null
	chmod 644 ca.pem cert.pem key.pem
)

# tls_ok checks that host:port serves a certificate the CA verifies (STARTTLS when $3 is set).
tls_ok() {
	local args=(-connect "$1:$2" -CAfile "$work/tls/ca.pem" -verify_return_error -verify_ip 127.0.0.1)
	[ -n "${3:-}" ] && args+=(-starttls ldap)
	openssl s_client "${args[@]}" </dev/null 2>&1 | grep -q "Verify return code: 0"
}

docker rm -f "$openldap_name" "$samba_name" "$postgres_name" >/dev/null 2>&1 || true

if has openldap; then
	log "OpenLDAP ($openldap_image)"
	docker run -d --name "$openldap_name" \
		-p "127.0.0.1:$openldap_port:389" -p "127.0.0.1:$openldap_ldaps_port:636" \
		-e LDAP_ORGANISATION=Example -e LDAP_DOMAIN="$ol_domain" -e LDAP_ADMIN_PASSWORD="$ol_admin_pw" \
		-e LDAP_READONLY_USER=true -e LDAP_READONLY_USER_USERNAME=readonly -e LDAP_READONLY_USER_PASSWORD="$ol_readonly_pw" \
		-e LDAP_TLS_CRT_FILENAME=cert.pem -e LDAP_TLS_KEY_FILENAME=key.pem -e LDAP_TLS_CA_CRT_FILENAME=ca.pem \
		-e LDAP_TLS_VERIFY_CLIENT=never -e LDAP_TLS_ENFORCE=false \
		-v "$work/tls:/container/service/slapd/assets/certs:ro" \
		"$openldap_image" --copy-service >/dev/null
	wait_for 180 "OpenLDAP" docker exec "$openldap_name" ldapsearch -x -H ldap://localhost -b "$ol_base" -s base \
		-D "cn=admin,$ol_base" -w "$ol_admin_pw"
	cat >"$work/openldap.ldif" <<EOF
dn: ou=people,$ol_base
objectClass: organizationalUnit
ou: people

dn: ou=groups,$ol_base
objectClass: organizationalUnit
ou: groups

dn: uid=$ol_member,ou=people,$ol_base
objectClass: inetOrgPerson
uid: $ol_member
cn: Jane Doe
sn: Doe
mail: $ol_member@$ol_domain
userPassword: $ol_member_pw

dn: uid=$ol_other,ou=people,$ol_base
objectClass: inetOrgPerson
uid: $ol_other
cn: Bob Roe
sn: Roe
mail: $ol_other@$ol_domain
userPassword: $ol_other_pw

dn: cn=hangar-publishers,ou=groups,$ol_base
objectClass: groupOfNames
cn: hangar-publishers
member: uid=$ol_member,ou=people,$ol_base

dn: cn=staff,ou=groups,$ol_base
objectClass: groupOfNames
cn: staff
member: uid=$ol_member,ou=people,$ol_base
member: uid=$ol_other,ou=people,$ol_base
EOF
	# -c: entries that exist already (code 68) are skipped.
	docker exec -i "$openldap_name" ldapadd -x -c -H ldap://localhost -D "cn=admin,$ol_base" -w "$ol_admin_pw" \
		<"$work/openldap.ldif" >/dev/null || [ $? -eq 68 ]
	wait_for 60 "OpenLDAP StartTLS with the e2e certificate" tls_ok 127.0.0.1 "$openldap_port" starttls
	wait_for 60 "OpenLDAP ldaps with the e2e certificate" tls_ok 127.0.0.1 "$openldap_ldaps_port"
fi

if has ad; then
	log "Samba Active Directory domain controller ($samba_image)"
	docker run -d --privileged --name "$samba_name" --hostname dc1 \
		-e REALM="$ad_realm" -e DOMAIN="$ad_domain" -e ADMIN_PASS="$ad_admin_pw" \
		-e DNS_FORWARDER="${LDAP_E2E_DNS_FORWARDER:-8.8.8.8}" -e BIND_NETWORK_INTERFACES=false \
		-p "127.0.0.1:$samba_port:389" -p "127.0.0.1:$samba_ldaps_port:636" \
		"$samba_image" >/dev/null
	samba_up() { docker exec "$samba_name" bash -c 'exec 3<>/dev/tcp/127.0.0.1/636' && docker exec "$samba_name" samba-tool user list; }
	wait_for 300 "the domain controller to provision and listen" samba_up
	st() { docker exec "$samba_name" samba-tool "$@"; }
	user() { # user <name> <password> [samba-tool options]
		local name=$1 pw=$2
		shift 2
		st user show "$name" >/dev/null 2>&1 || st user create "$name" "$pw" --use-username-as-cn "$@" >/dev/null
		st user setexpiry "$name" --noexpiry >/dev/null
	}
	group() { st group show "$1" >/dev/null 2>&1 || st group add "$1" >/dev/null; }
	member() { st group listmembers "$1" 2>/dev/null | grep -qx "$2" || st group addmembers "$1" "$2" >/dev/null; }
	user "$ad_svc" "$ad_svc_pw" --description="Hangar directory search"
	user "$ad_member" "$ad_member_pw" --given-name=Jane --surname=Doe --mail-address="$ad_member@samdom.example.com"
	user "$ad_other" "$ad_other_pw" --given-name=Bob --surname=Roe --mail-address="$ad_other@samdom.example.com"
	# Nested: ad-jdoe -> platform -> hangar-publishers (the mapped group).
	group hangar-publishers
	group platform
	member hangar-publishers platform
	member platform "$ad_member"
	# The e2e certificate replaces the generated one (Samba reads
	# private/tls/{key,cert,ca}.pem), then the DC restarts to load it.
	docker exec "$samba_name" mkdir -p /usr/local/samba/private/tls
	for f in key.pem cert.pem ca.pem; do
		docker cp "$work/tls/$f" "$samba_name:/usr/local/samba/private/tls/$f"
	done
	docker exec "$samba_name" bash -c 'cd /usr/local/samba/private/tls && chown root:root key.pem cert.pem ca.pem && chmod 600 key.pem && chmod 644 cert.pem ca.pem'
	docker restart "$samba_name" >/dev/null
	wait_for 180 "the domain controller after the restart" samba_up
	wait_for 120 "Samba ldaps with the e2e certificate" tls_ok 127.0.0.1 "$samba_ldaps_port"
	wait_for 60 "Samba StartTLS with the e2e certificate" tls_ok 127.0.0.1 "$samba_port" starttls
fi

if [ -z "${HANGAR_E2E_URL:-}" ]; then
	command -v go >/dev/null || die "go is required to build hangar (or set HANGAR_E2E_URL)"
	log "PostgreSQL ($postgres_image)"
	docker run -d --name "$postgres_name" -p "127.0.0.1:$postgres_port:5432" \
		-e POSTGRES_USER=hangar -e POSTGRES_PASSWORD=hangar -e POSTGRES_DB=hangar "$postgres_image" >/dev/null
	wait_for 60 "PostgreSQL" docker exec "$postgres_name" pg_isready -U hangar -d hangar
	log "hangar serve on 127.0.0.1:$hangar_port"
	(cd "$root" && go build -o "$work/hangar" ./cmd/hangar)
	admin_pw=E2e-Root-$(openssl rand -hex 8)
	env HANGAR_SERVER_ADDR="127.0.0.1:$hangar_port" HANGAR_SERVER_BASE_URL="http://127.0.0.1:$hangar_port" \
		HANGAR_DATABASE_URL="postgres://hangar:hangar@127.0.0.1:$postgres_port/hangar?sslmode=disable" \
		HANGAR_DATABASE_MIGRATE_ON_START=true \
		HANGAR_STORAGE_FS_PATH="$work/blobs" HANGAR_NATS_DATA_DIR="$work/nats" \
		HANGAR_SECRET_KEY="$(openssl rand -base64 32)" HANGAR_DOCKER_TOKEN_KEY="$(openssl rand -base64 48)" \
		HANGAR_BOOTSTRAP_ADMIN_USER=admin HANGAR_BOOTSTRAP_ADMIN_PASSWORD="$admin_pw" \
		HANGAR_BOOTSTRAP_REQUIRE_PASSWORD_CHANGE=false \
		HANGAR_AUTH_SESSION_INSECURE=true HANGAR_SCANNING_ENABLED=false \
		"$work/hangar" serve >"$work/hangar.log" 2>&1 &
	hangar_pid=$!
	hangar_up() { kill -0 "$hangar_pid" && curl -fsS "http://127.0.0.1:$hangar_port/healthz"; }
	wait_for 120 "hangar (log: $work/hangar.log)" hangar_up
	export HANGAR_E2E_URL="http://127.0.0.1:$hangar_port"
	export HANGAR_E2E_ADMIN_USER=admin HANGAR_E2E_ADMIN_PASSWORD="$admin_pw"
fi

export HANGAR_E2E_LDAP_CA="$work/tls/ca.pem"
if has openldap; then
	export HANGAR_E2E_OPENLDAP_URL="ldap://127.0.0.1:$openldap_port" HANGAR_E2E_OPENLDAP_LDAPS_URL="ldaps://127.0.0.1:$openldap_ldaps_port"
	export HANGAR_E2E_OPENLDAP_BASE_DN="$ol_base" HANGAR_E2E_OPENLDAP_BIND_DN="cn=readonly,$ol_base"
	export HANGAR_E2E_OPENLDAP_BIND_PASSWORD="$ol_readonly_pw"
	export HANGAR_E2E_OPENLDAP_MEMBER="$ol_member" HANGAR_E2E_OPENLDAP_MEMBER_PASSWORD="$ol_member_pw"
	export HANGAR_E2E_OPENLDAP_OTHER="$ol_other" HANGAR_E2E_OPENLDAP_OTHER_PASSWORD="$ol_other_pw"
fi
if has ad; then
	export HANGAR_E2E_AD_URL="ldap://127.0.0.1:$samba_port" HANGAR_E2E_AD_LDAPS_URL="ldaps://127.0.0.1:$samba_ldaps_port"
	export HANGAR_E2E_AD_BASE_DN="$ad_base" HANGAR_E2E_AD_BIND_DN="CN=$ad_svc,CN=Users,$ad_base"
	export HANGAR_E2E_AD_BIND_PASSWORD="$ad_svc_pw"
	export HANGAR_E2E_AD_MEMBER="$ad_member" HANGAR_E2E_AD_MEMBER_PASSWORD="$ad_member_pw"
	export HANGAR_E2E_AD_OTHER="$ad_other" HANGAR_E2E_AD_OTHER_PASSWORD="$ad_other_pw"
fi

log "go test -tags e2e -run TestLDAP ./test/e2e/ against $HANGAR_E2E_URL"
cd "$root"
go test -tags e2e -count=1 -run '^TestLDAP$' -v "$@" ./test/e2e/
