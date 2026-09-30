#!/usr/bin/env bash
# 서버 API 시연: 로그인 → Party 생성 → 초대 → 미리보기 → 수락 → 멤버 목록 → 방장 위임.
# scripts/run_api_dev.sh로 띄운 서버(기본 http://localhost:8080)를 대상으로 한다.
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
SUFFIX="$(date +%s)"
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# api METHOD PATH TOKEN [BODY] [IF_MATCH] -> 본문 출력, HTTP 상태는 마지막 줄
api() {
  local method="$1" path="$2" token="${3:-}" body="${4:-}" ifmatch="${5:-}"
  local args=(-sS -X "$method" "$BASE$path" -H "Content-Type: application/json" -w '\n%{http_code}')
  [[ -n "$token" ]] && args+=(-H "Authorization: Bearer $token")
  [[ "$method" != "GET" ]] && args+=(-H "Idempotency-Key: $(uuidgen)")
  [[ -n "$ifmatch" ]] && args+=(-H "If-Match: \"$ifmatch\"")
  [[ -n "$body" ]] && args+=(-d "$body")
  curl "${args[@]}"
}
# 응답에서 상태를 검사하고 본문만 돌려준다.
expect() {
  local want="$1"; shift
  local out; out="$(api "$@")"
  local code="${out##*$'\n'}" body="${out%$'\n'*}"
  if [[ "$code" != "$want" ]]; then echo "기대 $want, 실제 $code: $body" >&2; exit 1; fi
  printf '%s' "$body"
}
login() { expect 200 POST /v1/auth/apple "" "{\"identity_token\":\"dev:$1\",\"authorization_code\":\"dev\",\"raw_nonce\":\"dev\",\"display_name\":\"$2\"}"; }

step "1. 두 사용자 로그인 (개발용 로그인)"
ALICE="$(login "alice.$SUFFIX" 앨리스)"; BOB="$(login "bob.$SUFFIX" 밥)"
AT="$(jq -r .access_token <<<"$ALICE")"; BT="$(jq -r .access_token <<<"$BOB")"
echo "앨리스 user_id=$(jq -r .user_id <<<"$ALICE")"; echo "밥     user_id=$(jq -r .user_id <<<"$BOB")"
BOB_ID="$(jq -r .user_id <<<"$BOB")"
# 밥의 sync 시작점. 이 뒤에 생기는 변경을 증분 sync로 받아 본다.
WM="$(expect 200 GET /v1/sync/bootstrap "$BT" | jq -r .cursor_watermark)"

step "2. 앨리스가 Party 생성"
P="$(expect 201 POST /v1/parties "$AT" '{"name":"주말 브런치 모임"}')"
PID="$(jq -r .party.id <<<"$P")"; jq -c '.party|{id,name,version,member_limit}' <<<"$P"

step "3. 앨리스가 초대 링크 생성 (token은 이 응답에만 나온다)"
I="$(expect 201 POST "/v1/parties/$PID/invites" "$AT" '{"max_uses":3}')"
TOKEN="$(jq -r .invite.token <<<"$I")"; jq -c '.invite|{status,max_uses,expires_at}' <<<"$I"; echo "token 길이: ${#TOKEN}"

step "4. 밥이 미리보기 (Party 이름·인원·초대자 이름만 보인다)"
expect 200 GET "/v1/invites/$TOKEN/preview" "$BT" | jq -c .

step "5. 밥이 수락"
expect 201 POST "/v1/invites/$TOKEN/accept" "$BT" | jq -c '.membership|{role,status}'

step "6. 멤버 목록 (앨리스 시점)"
expect 200 GET "/v1/parties/$PID/memberships" "$AT" | jq -c '.memberships[]|{display_name,role}'

step "7. 존재하지 않는 token으로 미리보기 → 만료·무효·미존재를 구분하지 않는 404"
expect 404 GET "/v1/invites/not-a-real-token-0123456789012345678901234/preview" "$BT" | jq -c .code

step "8. 앨리스가 밥에게 방장 위임"
V="$(jq -r .party.version <<<"$(expect 200 GET "/v1/parties/$PID" "$AT")")"
expect 200 POST "/v1/parties/$PID/owner" "$AT" "{\"user_id\":\"$BOB_ID\"}" "$V" | jq -c '.party|{version,owner_membership_id}'

step "9. 위임 뒤: 앨리스는 더 이상 초대를 만들 수 없고(403), 밥은 만들 수 있다"
expect 403 POST "/v1/parties/$PID/invites" "$AT" '{}' | jq -c .code
expect 201 POST "/v1/parties/$PID/invites" "$BT" '{}' | jq -c '.invite|{status,max_uses}'

# --- 캘린더: 기기가 올린 시간만 서버에 있고, 다른 멤버에게는 "일정 있음" 시간대만 보인다 ---
rand_key() { openssl rand -base64 16 | tr '+/' '-_' | tr -d '='; }   # 16바이트 무작위 source_event_key
day() { date -u -v+"$1"d +%Y-%m-%dT00:00:00Z 2>/dev/null || date -u -d "+$1 day" +%Y-%m-%dT00:00:00Z; }
at() { date -u -j -f %Y-%m-%dT%H:%M:%SZ -v+"$2"H "$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "$1 +$2 hour" +%Y-%m-%dT%H:%M:%SZ; }
upload_calendar() {   # upload_calendar TOKEN REVISION START1 START2 ...
  local tok="$1" rev="$2"; shift 2
  local ws we facts="[" first=1
  ws="$(day 0)"; we="$(day 30)"
  for st in "$@"; do
    [[ $first -eq 0 ]] && facts+=","; first=0
    facts+="{\"source_event_key\":\"$(rand_key)\",\"start_at\":\"$st\",\"end_at\":\"$(at "$st" 1)\",\"all_day\":false,\"time_zone\":\"Asia/Seoul\",\"availability\":\"busy\"}"
  done
  facts+="]"
  local snap sid
  snap="$(expect 201 POST /v1/calendar/snapshots "$tok" "{\"revision\":$rev,\"window_start\":\"$ws\",\"window_end\":\"$we\",\"expected_pages\":1}")"
  sid="$(jq -r .snapshot.id <<<"$snap")"
  curl -sS -o /dev/null -X PUT "$BASE/v1/calendar/snapshots/$sid/pages/0" -H "Authorization: Bearer $tok" -H "Content-Type: application/json" -d "{\"facts\":$facts}"
  expect 200 POST "/v1/calendar/snapshots/$sid/complete" "$tok" "{\"fact_count\":$#}" | jq -c '.connection|{status,active_generation}'
}
connect_calendar() {
  expect 200 PUT /v1/calendar/connection "$1" '{"status":"selecting","sources":[{"key":"calendar-1","enabled":true}],"time_zone":"Asia/Seoul","claim_sync_device":true}' >/dev/null
}

step "10. 앨리스와 밥이 캘린더를 연결하고 일정 시간대를 업로드 (제목·장소 필드는 서버가 받지도 않는다)"
connect_calendar "$AT"; connect_calendar "$BT"
echo -n "앨리스: "; upload_calendar "$AT" 1 "$(at "$(day 2)" 10)" "$(at "$(day 3)" 14)"
echo -n "밥:     "; upload_calendar "$BT" 1 "$(at "$(day 2)" 13)"

step "11. 밥의 증분 sync에 내려온 일정 (시작·끝·종일·시간대뿐. 제목·장소·source key 없음)"
expect 200 GET "/v1/sync?cursor=$WM" "$BT" \
  | jq -c '.changes[]|select(.entity_type=="party_schedule_projection")|.payload|{owner_user_id,visibility_level,start_at,end_at}'

step "12. 제목을 올리려 하면 서버가 거부한다 (allowlist)"
S="$(expect 201 POST /v1/calendar/snapshots "$AT" "{\"revision\":2,\"window_start\":\"$(day 0)\",\"window_end\":\"$(day 30)\",\"expected_pages\":1}")"
curl -sS -X PUT "$BASE/v1/calendar/snapshots/$(jq -r .snapshot.id <<<"$S")/pages/0" -H "Authorization: Bearer $AT" -H "Content-Type: application/json" \
  -d "{\"facts\":[{\"source_event_key\":\"$(rand_key)\",\"start_at\":\"$(at "$(day 4)" 9)\",\"end_at\":\"$(at "$(day 4)" 10)\",\"all_day\":false,\"time_zone\":\"Asia/Seoul\",\"availability\":\"busy\",\"title\":\"비밀 면접\"}]}" | jq -c .code

printf '\n\033[1m시연 완료\033[0m\n'
