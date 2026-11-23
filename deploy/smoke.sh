#!/usr/bin/env bash
# Smoke test against a running deployment: deploy/smoke.sh <base-url> <admin-email> <admin-password>
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
exit $fail
