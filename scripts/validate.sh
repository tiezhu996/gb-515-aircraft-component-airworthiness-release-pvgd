#!/usr/bin/env sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
set -a
if [ -f .env ]; then . ./.env; else . ./.env.example; fi
set +a

(command -v jq >/dev/null 2>&1) || { echo "jq is required for API validation" >&2; exit 1; }

(cd backend && go test ./... && go test -race ./... && go vet ./... && go build ./...)
(cd frontend && npm install --no-audit --no-fund && npm run typecheck && npm run build)
docker compose config --quiet
docker compose down -v --remove-orphans
docker compose up -d --build

cleanup() { docker compose down -v --remove-orphans; }
if [ "${KEEP_RUNNING:-0}" = "1" ]; then
  trap cleanup INT TERM
else
  trap cleanup EXIT INT TERM
fi

i=0
until curl -fsS "http://127.0.0.1:${BACKEND_PORT:-19515}/healthz" | jq -e '.data.status == "ok" and .data.database == "ready" and .data.redis == "ready"' >/dev/null; do
  i=$((i+1))
  [ "$i" -lt 60 ] || { docker compose logs; exit 1; }
  sleep 2
done
curl -fsS "http://127.0.0.1:${FRONTEND_PORT:-18515}/" >/dev/null

login_token() {
  curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT:-19515}/api/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"Admin123!\"}" | jq -er '.data.token'
}

admin_token=$(login_token admin)
reviewer_token=$(login_token reviewer)
operator_token=$(login_token operator)
viewer_token=$(login_token viewer)

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/session" -H "Authorization: Bearer $viewer_token" \
  | jq -e '.data.role == "viewer" and (.data.requestId | length > 0)' >/dev/null
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/parts?page=1&pageSize=20" -H "Authorization: Bearer $viewer_token" \
  | jq -e '.data | length >= 3' >/dev/null

viewer_write_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts" \
  -H "Authorization: Bearer $viewer_token" -H 'Content-Type: application/json' -d '{}')
[ "$viewer_write_status" = "403" ]

now=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
suffix=$(date +%s)

authorization_payload=$(printf '{"code":"AUTH-SMOKE-%s","name":"Validated component release","description":"Dual-control Compose validation","facility":"Validation Hangar","owner":"Release Desk","category":"engine","riskLevel":"high","metricValue":100,"metricUnit":"percent","effectiveAt":"%s","evidence":"inspection IR-SMOKE and certificate CERT-SMOKE","relatedCode":"PART-SMOKE"}' "$suffix" "$now")
authorization=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-create' \
  -d "$authorization_payload")
authorization_id=$(printf '%s' "$authorization" | jq -er '.data.id')
authorization_version=$(printf '%s' "$authorization" | jq -er '.data.version')
printf '%s' "$authorization" | jq -e '.data.status == "draft" and .data.version == 1 and .data.revisions[0].actor == "operator" and .data.revisions[0].requestId == "gb515-auth-create"' >/dev/null

authorization_review=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-review' \
  -d "{\"status\":\"review\",\"expectedVersion\":${authorization_version},\"reason\":\"inspection and certificate evidence complete\"}")
authorization_review_version=$(printf '%s' "$authorization_review" | jq -er '.data.version')
printf '%s' "$authorization_review" | jq -e '.data.status == "review" and .data.submittedBy == "operator" and (.data.revisions | length) == 2' >/dev/null

operator_approval_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-operator-denied' \
  -d "{\"status\":\"approved\",\"expectedVersion\":${authorization_review_version},\"reason\":\"operator must not self approve\"}")
[ "$operator_approval_status" = "403" ]

authorization_approved=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${authorization_id}/transition" \
  -H "Authorization: Bearer $reviewer_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-auth-approve' \
  -d "{\"status\":\"approved\",\"expectedVersion\":${authorization_review_version},\"reason\":\"independent airworthiness release review passed\"}")
printf '%s' "$authorization_approved" | jq -e '.data.status == "approved" and .data.version == 3 and .data.submittedBy == "operator" and .data.reviewedBy == "reviewer" and (.data.revisions | length) == 3 and .data.revisions[2].requestId == "gb515-auth-approve"' >/dev/null

certificate_payload=$(printf '{"code":"CERT-SMOKE-%s","name":"Validated airworthiness certificate","description":"Immutable certificate validation","facility":"Validation Hangar","owner":"Certificate Desk","category":"engine","riskLevel":"medium","metricValue":100,"metricUnit":"percent","effectiveAt":"%s","evidence":"inspection report IR-SMOKE","relatedCode":"PART-SMOKE"}' "$suffix" "$now")
certificate=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/certificates" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-cert-create' \
  -d "$certificate_payload")
certificate_id=$(printf '%s' "$certificate" | jq -er '.data.id')
certificate_version=$(printf '%s' "$certificate" | jq -er '.data.version')

operator_certificate_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/certificates/${certificate_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-cert-operator-denied' \
  -d "{\"status\":\"valid\",\"expectedVersion\":${certificate_version},\"reason\":\"operator must not publish\"}")
[ "$operator_certificate_status" = "403" ]

certificate_valid=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/certificates/${certificate_id}/transition" \
  -H "Authorization: Bearer $reviewer_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-cert-publish' \
  -d "{\"status\":\"valid\",\"expectedVersion\":${certificate_version},\"reason\":\"independent certificate evidence review passed\"}")
printf '%s' "$certificate_valid" | jq -e '.data.status == "valid" and .data.version == 2 and .data.preparedBy == "operator" and .data.verifiedBy == "reviewer" and (.data.revisions | length) == 2 and .data.revisions[1].requestId == "gb515-cert-publish"' >/dev/null

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/audits/ReleaseAuthorization/${authorization_id}?limit=10" \
  -H "Authorization: Bearer $admin_token" \
  | jq -e '[.data[].requestId] | index("gb515-auth-create") != null and index("gb515-auth-review") != null and index("gb515-auth-approve") != null' >/dev/null

# --- Airworthiness blocking closure -----------------------------------------
# A failed inspection must atomically hold the same-code part, force
# review/approved authorizations to restricted (recording task code + reason),
# block release/approval/resubmission, and after re-inspection passes allow
# exactly one re-submission for the unchanged dual-control review.
block_now=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
block_suffix=$(date +%s)
block_code="BLOCK-SMOKE-${block_suffix}"
block_part=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-part' \
  -d "{\"code\":\"PART-BLOCK-${block_suffix}\",\"name\":\"Block closure part\",\"facility\":\"Validation Hangar\",\"owner\":\"Release Desk\",\"category\":\"engine\",\"riskLevel\":\"critical\",\"metricValue\":100,\"metricUnit\":\"percent\",\"effectiveAt\":\"${block_now}\",\"evidence\":\"pre-failure evidence\",\"relatedCode\":\"${block_code}\"}")
block_part_id=$(printf '%s' "$block_part" | jq -er '.data.id')
block_part_v1=$(printf '%s' "$block_part" | jq -er '.data.version')
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"inspection\",\"expectedVersion\":${block_part_v1},\"reason\":\"enter inspection\"}" >/dev/null

block_inspection=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/inspections" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-it' \
  -d "{\"code\":\"IT-BLOCK-${block_suffix}\",\"name\":\"Block closure inspection\",\"facility\":\"Validation Hangar\",\"owner\":\"Release Desk\",\"category\":\"engine\",\"riskLevel\":\"critical\",\"metricValue\":100,\"metricUnit\":\"percent\",\"effectiveAt\":\"${block_now}\",\"evidence\":\"measurement sheet\",\"relatedCode\":\"${block_code}\"}")
block_inspection_id=$(printf '%s' "$block_inspection" | jq -er '.data.id')
block_inspection_v1=$(printf '%s' "$block_inspection" | jq -er '.data.version')
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/inspections/${block_inspection_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"running\",\"expectedVersion\":${block_inspection_v1},\"reason\":\"start inspection\"}" >/dev/null

block_auth=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-auth' \
  -d "{\"code\":\"AUTH-BLOCK-${block_suffix}\",\"name\":\"Block closure release\",\"facility\":\"Validation Hangar\",\"owner\":\"Release Desk\",\"category\":\"engine\",\"riskLevel\":\"critical\",\"metricValue\":100,\"metricUnit\":\"percent\",\"effectiveAt\":\"${block_now}\",\"evidence\":\"evidence pack\",\"relatedCode\":\"${block_code}\"}")
block_auth_id=$(printf '%s' "$block_auth" | jq -er '.data.id')
block_auth_v1=$(printf '%s' "$block_auth" | jq -er '.data.version')
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"review\",\"expectedVersion\":${block_auth_v1},\"reason\":\"submit before failure\"}" >/dev/null

block_failed=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/inspections/${block_inspection_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-fail' \
  -d "{\"status\":\"failed\",\"expectedVersion\":2,\"reason\":\"blade crack beyond allowed limit\"}")
block_task_code=$(printf '%s' "$block_failed" | jq -er '.data.code')
block_failed_v=$(printf '%s' "$block_failed" | jq -er '.data.version')
printf '%s' "$block_failed" | jq -e '.data.status == "failed" and .data.failureReason == "blade crack beyond allowed limit"' >/dev/null

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}" -H "Authorization: Bearer $viewer_token" \
  | jq -e --arg code "$block_task_code" '.data.status == "hold" and .data.blockActive == true and .data.blockingTaskCode == $code and .data.blockingReason == "blade crack beyond allowed limit"' >/dev/null
curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}" -H "Authorization: Bearer $viewer_token" \
  | jq -e --arg code "$block_task_code" '.data.status == "restricted" and .data.blockActive == true and .data.blockingTaskCode == $code and (.data.revisions[-1].action == "airworthiness_block")' >/dev/null

block_part_vhold=$(curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}" -H "Authorization: Bearer $operator_token" | jq -er '.data.version')
block_release_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"released\",\"expectedVersion\":${block_part_vhold},\"reason\":\"release during block\"}")
[ "$block_release_status" = "409" ]
block_auth_vrestricted=$(curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}" -H "Authorization: Bearer $operator_token" | jq -er '.data.version')
block_resubmit_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"review\",\"expectedVersion\":${block_auth_vrestricted},\"reason\":\"resubmit during block\"}")
[ "$block_resubmit_status" = "409" ]
block_duplicate_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/inspections/${block_inspection_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"failed\",\"expectedVersion\":${block_failed_v},\"reason\":\"duplicate judgment\"}")
[ "$block_duplicate_status" = "422" ]

curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/inspections/${block_inspection_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"running\",\"expectedVersion\":${block_failed_v},\"reason\":\"reopen for re-inspection\"}" >/dev/null
block_reopen_status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"review\",\"expectedVersion\":${block_auth_vrestricted},\"reason\":\"early resubmit during re-inspection\"}")
[ "$block_reopen_status" = "409" ]
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/inspections/${block_inspection_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-pass' \
  -d "{\"status\":\"passed\",\"expectedVersion\":$((block_failed_v + 1)),\"reason\":\"re-inspection passed\"}" >/dev/null

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}" -H "Authorization: Bearer $viewer_token" \
  | jq -e '.data.status == "hold" and .data.blockActive == false and (.data.blockingTaskCode | length > 0)' >/dev/null
block_recovered=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-resubmit' \
  -d "{\"status\":\"review\",\"expectedVersion\":$((block_auth_vrestricted + 1)),\"reason\":\"resubmit after re-inspection\"}")
printf '%s' "$block_recovered" | jq -e '.data.status == "review" and .data.submittedBy == "operator" and (.data.reviewedBy == "")' >/dev/null
block_review_v=$(printf '%s' "$block_recovered" | jq -er '.data.version')
block_self_approve=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"approved\",\"expectedVersion\":${block_review_v},\"reason\":\"operator self approval\"}")
[ "$block_self_approve" = "403" ]
block_reapproved=$(curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/authorizations/${block_auth_id}/transition" \
  -H "Authorization: Bearer $reviewer_token" -H 'Content-Type: application/json' -H 'X-Request-ID: gb515-block-reapprove' \
  -d "{\"status\":\"approved\",\"expectedVersion\":${block_review_v},\"reason\":\"independent re-approval after re-inspection\"}")
printf '%s' "$block_reapproved" | jq -e '.data.status == "approved" and .data.reviewedBy == "reviewer"' >/dev/null
block_part_vclear=$(curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}" -H "Authorization: Bearer $operator_token" | jq -er '.data.version')
curl -fsS -X POST "http://127.0.0.1:${BACKEND_PORT}/api/parts/${block_part_id}/transition" \
  -H "Authorization: Bearer $operator_token" -H 'Content-Type: application/json' \
  -d "{\"status\":\"released\",\"expectedVersion\":${block_part_vclear},\"reason\":\"manual release after recovery\"}" \
  | jq -e '.data.status == "released"' >/dev/null

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/audits/AircraftPart/${block_part_id}?limit=20" -H "Authorization: Bearer $admin_token" \
  | jq -e '[.data[].action] | index("airworthiness_block") != null and index("airworthiness_release") != null' >/dev/null

curl -fsS "http://127.0.0.1:${BACKEND_PORT}/api/audit-summary?windowHours=24" -H "Authorization: Bearer $admin_token" \
  | jq -e '.data.total >= 5 and .data.transitions >= 3 and .data.uniqueActors >= 2' >/dev/null

docker compose ps
[ "${KEEP_RUNNING:-0}" = "1" ] && echo "KEEP_RUNNING=1: containers left running for built-in Browser validation"
