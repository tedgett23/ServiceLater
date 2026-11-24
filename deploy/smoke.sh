#!/usr/bin/env bash
# Smoke test against a running deployment: deploy/smoke.sh <base-url> <admin-email> <admin-password>
# (SOURCE: the deployment's address as its own worker reaches it, for the clone pull; the base URL by default.)
# Each check prints ok or FAIL; the exit status is 1 if any failed. It files records, a problem, an attachment and a
# service account with a key, so run it against a test deployment, not production (make release-check does).
set -u
B=$1; J=$(mktemp); trap 'rm -f $J' EXIT
fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got $2, want $3"; fail=1; fi; }
req() { curl -sS -b $J -c $J -H 'Content-Type: application/json' -H "Origin: $B" "$@"; }
check "healthz" "$(curl -s -o /dev/null -w '%{http_code}' $B/healthz)" 200
check "the web UI" "$(curl -s $B/ | grep -q '_app/immutable/entry/start' && echo yes)" yes
check "sign in" "$(req -o /dev/null -w '%{http_code}' -X POST $B/api/v1/auth/login -d "{\"email\":\"$2\",\"password\":\"$3\"}")" 200
check "bundled apps (record types in use)" "$(req $B/api/v1/record-types | grep -o '"type":"[a-z_]*"' | sort | tr -d '\n')" '"type":"change""type":"incident""type":"problem""type":"request""type":"work_task"'
INC=$(req -X POST $B/api/v1/tickets -d '{"title":"Release smoke: VPN drops","description":"The concentrator reboots nightly"}')
ID=$(echo "$INC" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4); REF=$(echo "$INC" | grep -o '"reference":"[^"]*"' | cut -d'"' -f4)
check "file an incident" "$([ -n "$ID" ] && echo yes)" yes
check "an SLA clock started" "$(req $B/api/v1/tickets/$ID | grep -c '"slas":\[{')" 1
check "search by description" "$(req "$B/api/v1/search?q=concentrator" | grep -c "\"reference\":\"$REF\"")" 1
# SQL queries run on a login role of their own that the migrations create: this proves the database let them make it
# and lets the server sign in as it, and that the views see what was just filed. It uses the QUERY method (RFC 10008),
# so it also proves the deployment passes it through.
Q=$(req -X QUERY $B/api/v1/query -d "{\"sql\":\"SELECT reference FROM records WHERE id = \$1::uuid\",\"params\":[\"$ID\"]}")
check "a SQL query finds it" "$(echo "$Q" | grep -c "\"rows\":\[\[\"$REF\"\]\]")" 1
check "the query views" "$(req $B/api/v1/query/schema | grep -o '"name":"records"' | head -1)" '"name":"records"'
PRB=$(req -X POST $B/api/v1/tickets -d "{\"type\":\"problem\",\"title\":\"Release smoke problem\",\"incidents\":[\"$ID\"]}" | grep -o '"open_incidents":[0-9]*')
check "raise a problem linked to it" "$PRB" '"open_incidents":1'
# An attachment through the server's own upload URLs, into the volume.
A=$(req -X POST $B/api/v1/tickets/$ID/attachments -d '{"filename":"note.txt","size":5,"content_type":"text/plain"}')
AID=$(echo "$A" | grep -o '"attachment":{"id":"[^"]*"' | cut -d'"' -f6)
URL=$(req -X POST $B/api/v1/attachments/$AID/parts -d '{"numbers":[1]}' | grep -o '"url":"[^"]*"' | cut -d'"' -f4)
case $URL in http*) ;; *) URL=$B$URL;; esac
ETAG=$(curl -sS -X PUT --data-binary 'hello' -D - -o /dev/null "$URL" | grep -i '^etag:' | tr -d '\r"' | cut -d' ' -f2-)
check "upload a part" "$([ -n "$ETAG" ] && echo yes)" yes
check "complete the upload" "$(req -o /dev/null -w '%{http_code}' -X POST $B/api/v1/attachments/$AID/complete -d "{\"parts\":[{\"number\":1,\"etag\":\"$ETAG\"}]}")" 200
DL=$(req $B/api/v1/attachments/$AID/download | grep -o '"url":"[^"]*"' | cut -d'"' -f4); case $DL in http*) ;; *) DL=$B$DL;; esac
check "download it" "$(curl -sS "$DL")" hello
# An API key for a service account.
SA=$(req -X POST $B/api/v1/service-accounts -d '{"name":"Release smoke","role":"agent"}' | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
KEY=$(req -X POST $B/api/v1/users/$SA/api-keys -d '{"name":"smoke"}' | grep -o '"key":"[^"]*"' | cut -d'"' -f4)
check "an API key works" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" $B/api/v1/tickets)" 200
check "the API reference" "$(curl -s -o /dev/null -w '%{http_code}' $B/api/v1/openapi.json)" 200
# A snapshot: the worker takes it into the snapshots volume (which must be writable by the server's user), and the
# api serves it back. It needs SL_SECRETS_KEY, which the release check sets.
SNAP=$(req -X POST $B/api/v1/backups | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
for _ in $(seq 1 30); do req $B/api/v1/backups | grep -q "\"id\":\"$SNAP\",\"kind\":\"manual\",\"status\":\"ready\"" && break; sleep 1; done
check "take a snapshot" "$(req $B/api/v1/backups | grep -c "\"id\":\"$SNAP\",\"kind\":\"manual\",\"status\":\"ready\"")" 1
check "download the snapshot" "$(req $B/api/v1/backups/$SNAP/file | head -c 7)" SLSNAP1
# A clone package: a profile of open records with addresses masked, made by the worker into the same volume.
req -X POST $B/api/v1/clones/profiles -d '{"name":"smoke","config":{"records":"open","mask":{"emails":true}}}' >/dev/null
PKG=$(req -X POST $B/api/v1/clones/packages -d '{"profile":"smoke","passphrase":"smoke test passphrase","destination":"folder"}' | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
for _ in $(seq 1 30); do [ "$(req $B/api/v1/clones/packages/$PKG/file | head -c 8)" = SLCLONE1 ] && break; sleep 1; done
check "make a clone package" "$(req $B/api/v1/clones/packages/$PKG/file | head -c 8)" SLCLONE1
# Load it back into this (throwaway) instance, last, since it replaces everything: marked UAT, the package uploaded,
# then a fresh one pulled from this same instance as its own source (a pull key, the source's address as the worker
# sees it: the all-in-one container serves 8080 inside too), applied by the worker in maintenance, then sign in again
# (a clone signs everyone out) and the open incident is still there.
P=$(mktemp); trap 'rm -f $J $P' EXIT
req $B/api/v1/clones/packages/$PKG/file -o $P
req -X PUT $B/api/v1/settings/instance -d '{"name":"Smoke","environment":"uat"}' >/dev/null
UP=$(curl -sS -b $J -c $J -H 'Content-Type: application/octet-stream' -H "Origin: $B" --data-binary @$P $B/api/v1/clone-in/upload | grep -o '"status":"[a-z]*"' | head -1)
check "upload a clone package" "$UP" '"status":"uploaded"'
PK=$(req -X POST $B/api/v1/clones/profiles/smoke/keys -d '{"name":"Smoke"}' | grep -o '"key":"[^"]*"' | cut -d'"' -f4)
req -X PUT $B/api/v1/clone-in/source -d "{\"url\":\"${SOURCE:-$B}\",\"key\":\"$PK\",\"cron\":\"0 2 * * 0\",\"timezone\":\"UTC\",\"keep_people\":true,\"keep_studio\":true}" >/dev/null
check "the clone source answers" "$(req -X POST $B/api/v1/clone-in/source/test -d '{}' | grep -o '"profile":"smoke"')" '"profile":"smoke"'
LOAD=$(req -X POST $B/api/v1/clone-in/pull | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
for _ in $(seq 1 60); do req $B/api/v1/clone-in | grep -q "\"id\":\"$LOAD\",\"status\":\"uploaded\"" && break; sleep 1; done
check "pull a clone package" "$(req $B/api/v1/clone-in | grep -c "\"id\":\"$LOAD\",\"status\":\"uploaded\"")" 1
check "apply the pulled clone" "$(req -o /dev/null -w '%{http_code}' -X POST $B/api/v1/clone-in/$LOAD/apply -d '{"keep_people":true,"keep_studio":true,"confirm":"Smoke"}')" 202
for _ in $(seq 1 60); do curl -s $B/api/v1/instance | grep -q '"maintenance":null' && break; sleep 1; done
req -o /dev/null -X POST $B/api/v1/auth/login -d "{\"email\":\"$2\",\"password\":\"$3\"}"
check "the clone loaded" "$(req $B/api/v1/clone-in | grep -o '"status":"done"' | head -1)" '"status":"done"'
check "its records are there" "$(req $B/api/v1/tickets/$ID | grep -c "\"reference\":\"$REF\"")" 1
exit $fail
