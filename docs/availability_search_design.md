# 공통 가능 시간 검색 조건과 결과 표시 상세 설계

## 0. 설계 상태

- 상태: 확정
- 대상: Internal Alpha와 Launch MVP
- 순서: `docs/feature_design_backlog.md`의 상세 설계 진행 순서 6번
- 선행 설계: `docs/calendar_privacy_sync_design.md`, `docs/party_membership_design.md`, `docs/party_visibility_design.md`
- 후속 설계: 7(제안·응답·확정), 8(캘린더 쓰기·변경·취소), 9(알림)

이 설계는 모든 활성 멤버가 공통으로 가능한 시간을 찾기 위한 활동 시간, 검색 범위, 약속 길이, 슬롯 생성,
시간대, freshness, 결과 수와 정렬, API·캐시 계약을 확정한다. 일정 공개 수준은 검색 입력이 아니며 계산은 항상
private Busy facts를 사용한다.

### 이번 설계에서 확정한 제품 정책

| 항목 | 결정 | 근거 |
|---|---|---|
| 활동 시간 | 사용자별 주간 반복 활동 시간을 명시적으로 확인 | 캘린더가 비었다는 이유로 하루 전체를 가능으로 간주하지 않음 |
| 초기 상태 | 미설정. 매일 09:00~22:00 제안값은 사용자가 저장해야 효력 발생 | 안전한 기본과 빠른 설정을 함께 제공 |
| 기본 검색 | 오늘 포함 14일, 60분 약속 | 주말을 두 번 포함하면서 결과 과다를 억제 |
| 검색 한도 | 최대 31일, 약속 30분~8시간, 30분 단위 | privacy 탐색 표면과 계산 비용 제한 |
| 슬롯 기준 | 요청 표시 시간대의 `00`/`30`분 경계 | 사용자 화면에서 예측 가능한 시작 시각 제공 |
| 결과 수 | 최대 20개, 하루 최대 3개 | 첫날의 유사 슬롯이 결과를 독점하지 않게 함 |
| 정렬 | 날짜·시작 시각 오름차순 | 설명 가능하고 안정적인 MVP 정렬 |
| 공개 범위 | 전원 공통 슬롯만 반환 | 가능 인원과 불가능 원인을 통한 개인정보 추론 완화 |

---

## 1. 목표와 비목표

### 목표

1. 사용자가 검색 조건과 결과가 만들어진 이유를 예측할 수 있다.
2. 각 멤버의 활동 시간과 Busy fact의 교집합으로 전원 공통 슬롯만 계산한다.
3. 시간대와 DST 경계에서도 같은 요청이 안정적이고 재현 가능한 결과를 만든다.
4. 빈 결과와 준비 미완료를 구분해 잘못된 “가능 시간 없음” 표시를 막는다.
5. 멤버십·활동 시간·calendar generation 변화 뒤 오래된 결과를 재사용하지 않는다.

### 비목표

- 일부 멤버만 가능한 시간과 가능 인원 순위
- AI 추천, 이동 시간, 장소와 날씨 기반 정렬
- 개인별 선호 점수, 업무/개인 모드와 Party별 활동 시간 override
- 일정별 응답, 참석 가능성 예측과 자동 제안 발송
- 반복 약속 검색과 31일 초과 검색
- Google Calendar

---

## 2. 핵심 개념과 불변식

```text
각 활성 멤버의 주간 활동 시간 ─▶ 요청 날짜의 UTC 구간으로 확장
각 활성 멤버의 private Busy facts ─▶ 같은 사용자의 Busy 구간 병합
                                  │
활동 시간 - Busy ─────────────────┘
         │ 멤버별 free interval
         ▼
모든 활성 멤버의 교집합
         ▼
요청 표시 시간대 30분 grid에서 duration 슬롯 생성
         ▼
하루 최대 3개 · 전체 최대 20개로 안정 정렬
```

항상 유지해야 하는 불변식은 다음과 같다.

1. 검색 참여자는 요청 snapshot 시점의 모든 활성 Party 멤버다. 일부 제외는 Launch MVP에서 허용하지 않는다.
2. 모든 참여자가 활동 시간을 확인했고 calendar freshness gate를 통과해야 definitive 결과를 반환한다.
3. 일정 공개 수준과 Party projection 유무는 계산 결과를 바꾸지 않는다.
4. 빈 Busy facts는 활동 시간 전체가 가능함을 뜻할 수 있지만, 활동 시간 미설정은 가능으로 해석하지 않는다.
5. 후보 슬롯은 한 멤버의 활동 시간 밖에 있거나 Busy interval과 1초라도 겹치면 제외한다.
6. 응답에는 개인별 활동 시간, Busy 구간, 가능 여부, 제외 원인과 인원수가 없다.
7. 같은 입력 version과 파라미터는 같은 순서의 결과를 만든다.
8. 결과 version이 현재 Party/membership/activity/calendar version과 다르거나 freshness 유효기간이 지나면 제안 입력으로 사용할 수 없다.

---

## 3. 사용자별 활동 시간

### 3.1 초기 설정

- `availability_preferences`의 초기 상태는 `unconfigured`다.
- UI는 사용자 기기 시간대 기준 매일 09:00~22:00을 제안하지만 사용자가 `저장`해야 `configured`가 된다.
- 캘린더 연결 완료 화면과 첫 가능 시간 검색 전에 설정을 요청한다.
- 한 명이라도 미설정이면 검색은 빈 배열 대신 `availability_setup_pending`을 반환한다.
- 일반 멤버에게 미설정 사용자 ID나 수를 보여주지 않는다. 본인이 미설정이면 본인용 CTA를 함께 반환한다.

### 3.2 주간 반복 규칙

- 요일별 0~3개의 활동 interval을 저장한다. interval이 0개인 요일은 활동 불가다.
- 시작·종료는 사용자 기준 시간대의 30분 경계이며, 한 interval은 최소 30분이다.
- 같은 요일의 interval은 겹칠 수 없고 인접 interval은 서버가 병합한다.
- 자정을 넘는 입력은 시작 요일의 자정 전 구간과 다음 요일의 자정 후 구간으로 정규화한다.
- 정규화·병합 이후에도 요일별 최대 3개를 넘으면 `400 invalid_request`로 거부한다.
- 한 주 전체 interval이 0개인 설정은 `400 invalid_request`로 거부한다.
- Party별 override와 특정 날짜 예외는 Post-MVP다. 휴가·개별 예외는 캘린더의 Busy/all-day 일정으로 반영한다.

### 3.3 시간대

- 설정에는 IANA time zone ID를 저장한다. 고정 UTC offset은 허용하지 않는다.
- 기기 시간대가 바뀌면 사용자가 새 시간대로 활동 시간을 이동할지, 기존 지역 시각 의미를 유지할지 확인한다.
- 자동 변경하지 않으며 확인 전에는 기존 IANA time zone으로 계산한다.
- DST로 존재하지 않는 지역 시각은 해당 날짜의 다음 유효 시각으로 이동한다.
- DST 중복 시각은 활동 시작에는 첫 번째 occurrence, 종료에는 두 번째 occurrence를 사용해 활동 구간을 축소하지 않는다.
- 날짜별 UTC 확장 후 `normalized_start < normalized_end`를 다시 검사한다. spring-forward gap 때문에 길이가 0 이하가
  된 occurrence는 그 날짜에서 버리고, 모든 occurrence가 사라지면 그 날짜는 활동 불가로 처리한다.

---

## 4. 검색 입력과 검증

| 입력 | 기본값 | 허용 범위 |
|---|---|---|
| `start_date` | 요청 시간대의 오늘 | 오늘~90일 rolling snapshot 안 |
| `end_date` | 시작일 포함 14일째 | 시작일부터 최대 31일, snapshot 범위 안 |
| `duration_minutes` | 60 | 30~480, 30분 배수 |
| `time_zone` | 요청 기기의 IANA time zone | 유효한 IANA ID |
| `max_results` | 20 | 1~20 |

- 날짜 범위는 `start_date`와 `end_date`를 모두 포함한다.
- 과거 날짜를 보내면 `400 invalid_request`다. 오늘 검색에서는 이미 지난 시각만 후보에서 제외한다.
- 가장 이른 시작은 `max(현재 서버 시각 + 30분, start_date 00:00)`을 요청 시간대의 다음 30분 경계로 올림한 값이다.
- `end_date` 다음 날 00:00을 초과해 끝나는 슬롯은 반환하지 않는다.
- 90일 snapshot 밖 요청과 31일 초과 범위는 `400 search_range_invalid`다.
- 서버는 모든 활성 멤버의 active generation window를 UTC 반개구간으로 바꿔 교집합을 만든다. 요청 날짜 범위의
  UTC envelope 전체가 이 교집합 안에 있지 않으면 누락 구간을 free로 해석하지 않고 `400 search_range_invalid`를
  반환한다. 응답에는 어떤 멤버의 window가 부족한지 표시하지 않는다.
- 클라이언트가 멤버 ID, 공개 수준, 개별 활동 시간이나 정렬 점수를 입력으로 보내는 것을 허용하지 않는다.
- 검색 mutation이 아니므로 `Idempotency-Key`는 요구하지 않지만 request ID와 인증은 필수다.

---

## 5. 계산 알고리즘

### 5.1 입력 snapshot

서버는 한 요청에서 다음 version tuple을 고정해 읽는다.

```text
(party_version, membership_version_hash,
 activity_version_hash, calendar_generation_hash,
 fresh_until, search_parameters_hash)
```

- 계산은 PostgreSQL `REPEATABLE READ`, read-only transaction 하나에서 Party, 활성 membership, configured activity와
  active calendar generation을 읽어 일관된 snapshot을 만든다.
- 한 명이라도 membership, activity 또는 calendar 상태가 준비되지 않았으면 계산을 시작하지 않는다.
- `fresh_until`은 모든 참여자의 `last_completed_sync_at + 6시간` 중 가장 이른 값이다.
- 모든 active generation window의 UTC 교집합이 요청 envelope 전체를 덮는지 확인한 뒤 facts를 읽는다.
- serialization failure는 한 번 다시 시도하고, 다시 실패하면 `409 availability_changed_retry`를 반환한다.
- snapshot 이후 상태가 바뀔 수 있으므로 응답은 `calculationVersion`을 포함하고 제안 생성 시 현재 tuple과 다시 비교한다.

### 5.2 구간 계산

1. 각 멤버의 주간 활동 규칙을 해당 멤버의 time zone과 검색 날짜별 UTC interval로 확장한다.
2. 검색 범위와 겹치는 active generation의 private Busy facts를 읽는다.
3. 사용자별로 겹치거나 인접한 Busy interval을 병합한다.
4. 사용자별 활동 interval에서 Busy interval을 뺀다.
5. 모든 활성 멤버의 free interval 교집합을 계산한다.
6. 요청 `time_zone`의 각 날짜 `00`/`30`분 경계에서 `duration_minutes` 길이 후보를 생성한다.
7. 후보 전체가 공통 free interval 안에 있을 때만 유지한다.

interval은 반개구간 `[start, end)`으로 처리한다. Busy가 정확히 슬롯 종료에 시작하거나 슬롯 시작에 끝나면 겹치지 않는다.

### 5.3 결과 제한과 정렬

1. 유효 후보를 `start_at`, `end_at` 오름차순으로 안정 정렬한다.
2. 같은 날짜에서 시작이 가장 빠른 후보부터 최대 3개를 유지한다.
3. 남은 후보 전체에서 앞의 `max_results`개를 반환한다.

이 규칙은 같은 긴 free interval에서 30분씩 이동한 후보만 첫 화면을 채우는 문제를 줄이면서 별도 불투명 점수 없이
항상 같은 입력에 같은 결과를 만든다. 사용자가 `더 보기`를 요청할 때도 cursor pagination을 제공하지 않고 날짜 범위를
좁히거나 옮겨 새 검색을 실행한다.

---

## 6. Freshness와 준비 상태

| 상태 | 조건 | API 결과 |
|---|---|---|
| `ready` | 모든 멤버 활동 시간 configured + calendar ready + 마지막 정상 sync 6시간 이내 | `200` 슬롯 배열 또는 정상 빈 배열 |
| `availability_setup_pending` | 한 명 이상 활동 시간 미설정 | `409`, aggregate 상태 |
| `calendar_sync_pending` | 한 명 이상 calendar 비정상·stale | `409`, aggregate 상태 |
| `party_changed` | 계산 중 membership version 변경 | `409 availability_changed_retry` |

- 준비 미완료를 `200 []`로 반환하지 않는다. `[]`는 모든 준비 조건을 충족했지만 공통 슬롯이 없다는 뜻이다.
- 본인의 activity/calendar 문제는 self 상태에 구체적 CTA를 제공한다.
- 다른 멤버 문제는 `그룹 준비를 기다리는 중`으로만 표시하고 수, ID와 원인을 노출하지 않는다.
- 설계 7의 확정 직전 충돌 검사는 이 검색 결과와 별개로 15분 freshness를 다시 확인한다.

---

## 7. API 계약

### 7.1 활동 시간

| Method | Path | 권한 | 비고 |
|---|---|---|---|
| GET | `/v1/availability/preferences/me` | 인증 사용자 | 본인 규칙·시간대·version |
| PUT | `/v1/availability/preferences/me` | 인증 사용자 | 전체 주간 규칙 원자 교체, expected version·Idempotency-Key 필수 |

활동 시간은 사용자 전역 설정이다. 다른 사용자 규칙을 조회하는 endpoint는 만들지 않는다.

### 7.2 Party 검색

`POST /v1/parties/{party_id}/availability/search`

- 활성 멤버만 호출할 수 있다. 비멤버는 `404 not_found`다.
- 서버가 현재 Party의 모든 활성 멤버를 참여자로 고정한다.
- 사용자·Party 단위 분당 20회, 사용자 전체 분당 60회를 적용한다.

요청 예시:

```json
{
  "startDate": "2026-09-10",
  "endDate": "2026-09-23",
  "durationMinutes": 60,
  "timeZone": "Asia/Seoul",
  "maxResults": 20
}
```

성공 응답 예시:

```json
{
  "slots": [
    {
      "startAt": "2026-09-10T10:00:00Z",
      "endAt": "2026-09-10T11:00:00Z"
    }
  ],
  "calculationVersion": "opaque-hash",
  "generatedAt": "2026-09-10T05:00:00Z",
  "validUntil": "2026-09-10T05:05:00Z"
}
```

- `calculationVersion`은 version tuple의 keyed hash이며 내부 ID·version을 복원할 수 없는 opaque 값이다.
- `validUntil`은 `min(generatedAt + 5분, fresh_until)`이다. proposal 생성은 현재 tuple 일치와 서버 시각이
  `validUntil` 이전인지 모두 검사한다.
- 응답에 member count, available count, 원인, calendar 상태, activity interval과 visibility를 포함하지 않는다.
- 오류 응답도 문제가 있는 다른 멤버 수나 ID를 포함하지 않는다.

---

## 8. 결과 화면

- 기본 화면 제목은 `모두 가능한 시간`이다.
- 날짜별 section으로 묶고 요청 time zone의 지역 시각과 약식 time zone을 표시한다.
- 하루 최대 3개 결과를 시작 시각 오름차순으로 보여준다.
- 결과가 0개면 `선택한 기간과 활동 시간 안에 모두 가능한 시간이 없습니다`라고 표시하고 기간·길이 변경 CTA를 제공한다.
- 준비 미완료는 빈 결과 화면 대신 `그룹 준비를 기다리는 중` 상태를 표시한다.
- 본인 설정 문제일 때만 `내 활동 시간 설정`, `내 캘린더 새로고침` CTA를 노출한다.
- 슬롯 선택은 설계 7의 제안 작성 화면으로 이동하며 `calculationVersion`과 검색 파라미터를 함께 전달한다.
- 결과에 `4/5명 가능`, 멤버 아바타, 제외 사유와 특정 멤버의 캘린더 상태를 표시하지 않는다.

---

## 9. 캐시와 무효화

- 서버는 동일 Party·version tuple·검색 파라미터 결과를 `validUntil`까지만, 최대 5분 메모리 캐시할 수 있다.
  correctness가 캐시에 의존해서는 안 된다.
- membership, activity preference, active calendar generation 또는 Party 상태 변경 시 관련 cache key가 달라져 자동 miss가 난다.
- 클라이언트는 결과를 메모리에서 최대 5분 보관하되 앱 재시작 후 영구 캐시하지 않는다.
- Party sync에서 `party_version` 또는 membership hash가 바뀌면 화면의 결과를 즉시 만료시킨다.
- 본인 activity/calendar 변화도 결과를 즉시 만료시킨다.
- 슬롯으로 제안을 만들기 전 서버는 `calculationVersion`을 현재 tuple과 비교하고 `validUntil`을 재검사한다.
  다르거나 만료됐으면 `409 availability_result_stale`이며 재검색한다.

---

## 10. 실패와 복구

| 상황 | 처리 |
|---|---|
| 활동 시간 미설정 | `availability_setup_pending`; 본인만 설정 CTA |
| calendar denied/revoked/stale/error | `calendar_sync_pending`; 본인만 원인 CTA |
| 검색 중 version 변경 | 1회 재시도 후 `availability_changed_retry` |
| 시간대 ID 오류 | `400 invalid_request` |
| DST gap/overlap | §3.3 규칙으로 interval 정규화 |
| 요청 envelope가 멤버 generation window 교집합 밖 | `400 search_range_invalid`, 멤버별 원인 비노출 |
| 계산 timeout | `503 availability_temporarily_unavailable`, 동일 조건 재시도 허용 |
| rate limit | `429 rate_limited`, aggregate retry-after |
| 결과 version 만료 | `409 availability_result_stale`, 재검색 |
| 공통 슬롯 없음 | `200`과 빈 `slots`; 조건 변경 안내 |

계산 timeout과 내부 오류 시 부분 결과를 반환하지 않는다. 일부 멤버를 제외한 결과로 조용히 강등하지 않는다.

---

## 11. 데이터 모델

### `availability_preferences`

| 필드 | 계약 |
|---|---|
| `user_id` | primary key |
| `status` | `unconfigured`, `configured` |
| `time_zone` | IANA ID, configured일 때 필수 |
| `version`, `updated_at` | 변경 추적 |

### `availability_weekly_intervals`

| 필드 | 계약 |
|---|---|
| `user_id`, `weekday` | preference 소유자와 ISO weekday 1~7 |
| `start_minute`, `end_minute` | 0~1440, 30분 배수, start < end |
| `position` | 요일 안 안정 정렬, 0~2 check constraint |

- unique `(user_id, weekday, position)`을 둔다.
- `btree_gist`와 `int4range(start_minute, end_minute, '[)')` exclusion constraint로 같은 사용자·요일의 overlap을
  DB에서도 거부한다. service는 저장 전에 정규화·병합해 인접 구간을 제거한다.
- PUT은 기존 interval을 전체 교체하고 preference version 증가와 sync invalidation을 같은 transaction에서 기록한다.
- 원시 activity interval은 다른 사용자 sync에 발행하지 않는다.

---

## 12. 관측성과 개인정보

### 허용 지표

- 검색 요청 수, latency, timeout/error code와 반환 슬롯 수
- 요청 기간 일수, duration bucket, 결과 0건 비율
- activity setup 완료율과 설정 변경 수
- cache hit rate와 version 재시도 수

### 금지 데이터

- 사용자별 주간 활동 interval과 time zone을 Party/다른 사용자 ID와 결합한 로그
- 검색의 정확한 start/end timestamp와 반환 슬롯 timestamp
- Busy interval, 충돌 owner, 미설정/stale 사용자의 ID와 수
- hidden 여부와 availability 결과의 결합 분석

기간은 `1~7`, `8~14`, `15~31일`, duration은 `30`, `60`, `90~120`, `150분 이상` bucket으로만 집계한다.
최소 집계 크기 20 미만 cohort는 분석 결과에 내보내지 않는다. APNs에는 검색 조건과 결과를 포함하지 않는다.

---

## 13. 인수 조건

### 활동 시간

- [ ] 미설정 사용자는 suggested 09:00~22:00을 저장하기 전까지 계산 준비 완료로 간주되지 않는다.
- [ ] 요일별 interval overlap, 30분 미만과 30분 경계가 아닌 입력이 거부된다.
- [ ] 자정 횡단 입력이 두 요일 구간으로 정규화되고 결과가 보존된다.
- [ ] DST gap과 overlap 날짜에서 활동 시간이 §3.3 규칙대로 UTC로 변환된다.
- [ ] spring-forward 정규화 후 0/음수 길이 occurrence는 버리고 해당 날짜의 다른 구간에는 영향을 주지 않는다.
- [ ] 다른 사용자는 API/sync/로그로 원시 활동 시간을 얻지 못한다.

### 검색과 결과

- [ ] 기본 검색은 오늘 포함 14일, 60분, 최대 20개다.
- [ ] 31일 초과, 30분 미만, 8시간 초과와 30분 배수가 아닌 약속 길이가 거부된다.
- [ ] 요청 UTC envelope가 모든 active generation window 교집합에 포함되지 않으면 누락 구간을 free로 계산하지 않는다.
- [ ] 후보 시작은 요청 time zone의 00/30분 경계이고 과거·30분 lead time 이전 슬롯이 없다.
- [ ] 같은 날짜에는 최대 3개, 전체에는 `max_results` 이하만 시작 시각 순으로 반환된다.
- [ ] 모든 준비 조건이 충족된 빈 결과는 `200 []`, 준비 미완료는 `409`다.
- [ ] 응답과 오류에 member count, 개인별 가능 여부·원인·상태가 없다.

### 계산 정확성

- [ ] 활동 시간 밖 후보가 Busy facts가 없어도 반환되지 않는다.
- [ ] Busy가 슬롯 경계에 닿기만 할 때는 겹침으로 처리하지 않는다.
- [ ] details/busyOnly/hidden이 같은 private facts에서 동일한 결과를 만든다.
- [ ] 서로 다른 time zone 멤버의 주간 활동 시간이 UTC에서 정확히 교차한다.
- [ ] all-day, 반복 occurrence와 DST Busy가 공통 free interval에서 제외된다.
- [ ] 한 명의 Busy interval을 병합해도 결과가 중복되거나 누락되지 않는다.

### Version과 개인정보

- [ ] membership/activity/calendar generation 변화 후 기존 calculationVersion으로 제안을 시작할 수 없다.
- [ ] generation이 같아도 `validUntil`이 지나면 캐시를 반환하거나 제안을 시작할 수 없다.
- [ ] 검색 중 version이 연속 변경되면 부분·stale 결과 대신 retry 오류를 반환한다.
- [ ] 다른 멤버의 미설정·stale 상태는 수와 ID 없이 aggregate로만 보인다.
- [ ] 로그·trace·분석·APNs에 정확한 검색/결과 시간과 Busy 구간이 없다.

---

## 14. 검증 계획

### 순수 계산 단위 테스트

- 활동 interval - Busy interval 차집합과 멤버 교집합
- 반개구간 경계, 인접 Busy 병합, 슬롯 duration
- 30분 grid 정렬, 날짜당 3개·전체 20개 제한
- cross-time-zone, DST gap/overlap, 자정 횡단
- 기본/최소/최대 입력과 빈 결과

### 서버·PostgreSQL 통합 테스트

- activity PUT 전체 교체, version conflict와 interval 제약
- membership/activity/calendar version snapshot 일관성
- freshness gate와 retry-on-change
- cache key 무효화와 stale calculationVersion 거부
- rate limit과 timeout에서 부분 결과 비반환

### API·Privacy 계약 테스트

- 활성 멤버/비멤버 권한과 오류 존재 은닉
- ready/setup-pending/calendar-pending/empty 상태 schema
- 성공·오류 응답 금지 필드 검사
- raw activity interval의 sync 비발행
- 로그·trace·분석·APNs 시간·원인 키 스캔

### iOS 테스트

- 활동 시간 설정, suggestion 미확정 상태와 time zone 변경 확인
- 검색 form 기본값·validation과 결과 날짜 grouping
- 빈 결과와 준비 대기 상태 분리
- 결과 만료 후 제안 진입 차단과 재검색
- `AvailabilitySlot` count 필드 제거 뒤 화면 회귀

---

## 15. 현재 구현과의 차이

`src/PlanTogether/AvailabilityCalculator.swift`, `AppStore.swift`, `Models.swift` 기준이다.

| 현재 | 확정 설계 | 필요한 변경 |
|---|---|---|
| AppStore가 오늘 09:00~22:00, 60분으로 고정 | 사용자 활동 시간 + 14일 기본 검색 form | preferences/API/UI 추가 |
| 활동 시간 개념이 없음 | 명시적으로 확인한 사용자별 주간 interval | 모델·저장·time zone 변환 추가 |
| 검색 범위 하나에서 바로 슬롯 생성 | 날짜별 활동 구간 확장 후 멤버 교집합 | 계산 pipeline 분리 |
| cursor가 free interval 시작에서 duration만큼 이동 | 요청 time zone 00/30분 grid | slot generator 교체 |
| 결과 수 제한과 날짜 분산 없음 | 하루 최대 3개, 전체 최대 20개 | 안정 정렬·limit 추가 |
| 동기화 여부 부족 시 빈 배열 | setup/calendar pending은 409 상태 | 결과 enum/API 오류 분리 |
| count 두 필드 노출 | 전원 공통 start/end만 반환 | `AvailabilitySlot` 단순화 |
| 메모리 Party 데이터가 계산 version을 갖지 않음 | opaque calculationVersion 검증 | version tuple과 stale 처리 추가 |

이번 작업은 설계 확정 단계이므로 Swift 코드는 변경하지 않는다. 기존 프로젝트는 그대로 빌드·실행 가능한 상태를 유지한다.

---

## 16. 구현 순서

1. 기존 계산 테스트를 보존하고 activity·grid·limit 목표 동작을 실패 테스트로 추가한다.
2. activity preference/weekly interval migration과 self API를 구현한다.
3. time zone·DST activity expansion과 private Busy subtraction 순수 계산 모듈을 구현한다.
4. membership/activity/calendar version tuple과 freshness gate를 구현한다.
5. Party search API, 안정 정렬, rate limit과 5분 cache를 구현한다.
6. iOS activity 설정·검색 form·결과/대기/빈 상태를 구현한다.
7. calculationVersion을 설계 7의 제안 시작 경계에 연결한다.
8. §14 테스트와 privacy payload scan을 CI에 추가한다.

서버 골격, membership과 calendar private fact 저장이 선행한다. 순수 계산 모듈과 iOS 화면 모델 테스트는 서버 작업과
병행할 수 있다.

---

## 17. 후속 설계 제약

- 설계 7은 제안 생성 시 `calculationVersion`을 검증하되, 최신 availability를 확정 보장으로 간주하지 않고 충돌을 재검사한다.
- 떠난 참여자가 있는 제안의 처리 정책은 설계 7이 정하며 검색 참여자는 항상 현재 활성 멤버 전원이다.
- 설계 8의 외부 캘린더 쓰기 성공 여부는 과거 검색 결과를 변경하지 않는다. 다음 snapshot generation에서만 반영한다.
- 설계 9는 setup/calendar pending의 다른 멤버 원인과 ID를 푸시에 넣지 않는다.
- 부분 가능 시간과 선호 기반 ranking은 Post-MVP privacy review 전에는 현재 응답 schema에 추가하지 않는다.
