#!/usr/bin/env bash
# 두 시뮬레이터 시연 준비: 앱을 빌드해 두 시뮬레이터에 설치하고 캘린더 권한을 준 뒤 띄운다.
# 사전 조건: docker compose up -d --wait, scripts/run_api_dev.sh로 서버가 http://localhost:8080에 떠 있다.
#
# 사용: scripts/demo_ios_two_sims.sh ["iPhone 16" "iPhone 16 Pro"]
# 두 시뮬레이터는 같은 Mac에서 돌아 둘 다 localhost:8080에 닿는다.
#
# 시연 순서(시뮬레이터 A=앨리스, B=밥):
#   1. A: 개발용 로그인 alice / 앨리스.   B: bob / 밥
#   2. A: Party 탭 + → 새 Party → 상세에서 "초대 링크 만들기" → "코드 복사"
#      (시뮬레이터 간 클립보드는 공유되지 않는다. 터미널에서 `xcrun simctl pbpaste "$A" | xcrun simctl pbcopy "$B"`)
#   3. B: Party 탭 + → 초대 코드로 참여 → 붙여 넣기 → 확인 → 참여하기
#   4. 둘 다: 캘린더 탭 → 캘린더 연결 → 시연 일정 만들기(DEBUG) → "PlanTogether 시연" 켜기 → 서버 상태 ready
#   5. 둘 다: 활동 시간 탭 → 제안값 확인 → 저장
#   6. A: Party 상세 → 모두 가능한 시간 찾기 → 찾기
set -euo pipefail
cd "$(dirname "$0")/../../src"

A_NAME="${1:-iPhone 16}"
B_NAME="${2:-iPhone 16 Pro}"
BUNDLE=com.example.PlanTogether
BASE="${BASE:-http://localhost:8080}"

curl -sf "$BASE/readyz" >/dev/null || { echo "서버가 $BASE에 없다. scripts/run_api_dev.sh로 먼저 띄운다" >&2; exit 1; }

udid() { xcrun simctl list devices available | grep -F "    $1 (" | head -1 | sed -E 's/.*\(([0-9A-F-]{36})\).*/\1/'; }
A="$(udid "$A_NAME")"; B="$(udid "$B_NAME")"
[[ -n "$A" && -n "$B" && "$A" != "$B" ]] || { echo "시뮬레이터를 찾지 못했다: '$A_NAME'=$A '$B_NAME'=$B" >&2; exit 1; }

xcodegen generate >/dev/null
DERIVED="$(mktemp -d)"
xcodebuild -project PlanTogether.xcodeproj -scheme PlanTogether -configuration Debug \
  -destination "platform=iOS Simulator,id=$A" -derivedDataPath "$DERIVED" build -quiet
APP="$DERIVED/Build/Products/Debug-iphonesimulator/PlanTogether.app"

for d in "$A" "$B"; do
  xcrun simctl boot "$d" 2>/dev/null || true
  xcrun simctl bootstatus "$d" -b >/dev/null
  xcrun simctl install "$d" "$APP"
  xcrun simctl privacy "$d" grant calendar "$BUNDLE"
  SIMCTL_CHILD_PLAN_API_BASE_URL="$BASE" xcrun simctl launch --terminate-running-process "$d" "$BUNDLE" >/dev/null
done
open -a Simulator
echo "A($A_NAME)=$A, B($B_NAME)=$B 에 설치하고 띄웠다. 스크립트 머리말의 순서대로 시연한다."
echo "초대 코드 옮기기: xcrun simctl pbpaste $A | xcrun simctl pbcopy $B"
