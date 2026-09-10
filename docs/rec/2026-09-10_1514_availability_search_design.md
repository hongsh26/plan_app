# 공통 가능 시간 검색 조건과 결과 표시 설계 완료

## 확정한 정책

- 활동 시간은 사용자별 주간 반복 규칙이며 사용자가 명시적으로 저장해야 한다. UI의 매일 09:00~22:00은 제안값일 뿐 자동 적용하지 않는다.
- 기본 검색은 요청 시간대의 오늘부터 14일, 약속 길이 60분, 결과 최대 20개다.
- 검색 범위는 최대 31일, 약속 길이는 30분~8시간의 30분 배수다.
- 후보는 요청 표시 시간대의 00/30분 경계에서 만들고 날짜별 최대 3개를 시작 시각 순으로 반환한다.
- Launch MVP는 모든 활성 멤버가 가능한 슬롯만 반환한다. 일부 멤버 제외, 가능 인원과 개인별 원인은 Post-MVP다.

## 계산과 안전 경계

- 각 사용자의 지역 시간대 활동 구간을 UTC로 확장하고 private Busy facts를 뺀 뒤 모든 활성 멤버의 교집합을 구한다.
- 검색은 REPEATABLE READ, read-only transaction에서 Party·membership·activity·calendar generation version tuple을 고정한다.
- 모든 멤버의 active generation window UTC 교집합이 요청 envelope 전체를 덮지 않으면 누락 facts를 free로 해석하지 않고 요청을 거부한다.
- `fresh_until`은 참여자들의 마지막 정상 sync + 6시간 중 가장 이른 값이다. 응답 `validUntil`은 이 값과 5분 cache 한도 중 이른 값이다.
- `calculationVersion`이 현재 tuple과 다르거나 `validUntil`이 지나면 해당 슬롯으로 제안을 시작할 수 없다.
- 활동 시간 미설정과 calendar freshness 실패는 `409` aggregate pending이며, 준비가 끝난 정상 빈 결과 `200 []`와 구분한다.
- DST gap에서 정규화 후 길이가 0 이하인 활동 occurrence는 그 날짜에서 버린다.

## 데이터와 API

- `availability_preferences`에 상태·IANA time zone·version을 저장한다.
- `availability_weekly_intervals`는 요일별 최대 3개, 30분 경계이며 overlap은 `btree_gist` exclusion constraint로 막는다.
- 본인 활동 시간은 `/v1/availability/preferences/me`, Party 검색은 `/v1/parties/{party_id}/availability/search`가 소유한다.
- 다른 사용자 활동 시간, 미설정/stale 사용자 ID·수, 정확한 검색·결과 시각은 sync·로그·분석·APNs에 노출하지 않는다.

## 산출물

- 상세 설계: `docs/availability_search_design.md`
- 구현 계획: `.omc/plans/availability-search-implementation.md`
- 백로그: `docs/feature_design_backlog.md`의 `확정 설계 6`
- 진행 상황: 다음 작업을 설계 7로 이동

## 현재 구현과의 차이

- 현재는 당일 09:00~22:00, 60분으로 고정되어 사용자 활동 시간과 검색 form이 없다.
- 현재 계산은 free segment 시작부터 duration만큼 이동하며 요청 시간대 30분 grid를 사용하지 않는다.
- 결과 수·날짜별 제한, cross-time-zone/DST와 version snapshot이 없다.
- 미동기 상태와 정상 빈 결과를 모두 빈 배열로 표현한다.
- `AvailabilitySlot`은 개인정보 계약에서 금지한 가능/전체 인원수를 보유한다.

이번 작업은 문서 설계이므로 Swift·서버 코드는 변경하지 않았다.

## 검증

- 선행 calendar/privacy/membership/account 설계와 현재 계산기·단위 테스트를 대조했다.
- 별도 `code-reviewer` 에이전트가 Critical/High 0건, Medium 3건을 보고했다.
- 멤버별 snapshot window 경계, 시간 경과에 따른 freshness 만료, DST spring-forward의 0/음수 구간을 모두 설계·인수 조건·구현 계획에 반영했다.
- `git diff --check`와 활성 진행 항목의 stale reference 검색을 수행했다.
- 코드 변경이 없어 Xcode 빌드와 단위 테스트는 실행하지 않았다. 구현 시 기존 4개 테스트를 보존하고 설계 §14 테스트를 먼저 추가한다.

## 다음 작업

- 제안·응답·확정 상태 전이와 떠난 참여자가 포함된 제안의 처리 정책을 상세 설계한다. (설계 7)
