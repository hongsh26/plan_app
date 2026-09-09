# Party 생성·초대·역할·탈퇴 설계 완료

## 확정한 제품 정책

사용자 확인을 거쳐 스키마에 영향을 주는 정책 3건을 확정했다.

- 방장 탈퇴: 위임 후 탈퇴 강제. 다른 활성 멤버가 있으면 소유권을 넘겨야만 나갈 수 있고, 단독 멤버일 때만 탈퇴가 곧 해산이다. `parties.owner_membership_id` NOT NULL 불변식과 진행 중인 제안·확정 일정을 보존하기 위해 선택했다.
- 초대 방식: 재사용 가능한 Universal Link + 수락 시 앱 내 계정 필수. 서버에 pre-account 상태를 만들지 않아 스키마와 계정 삭제 경로가 단순하다.
- 멤버 상한: 활성 멤버 10명(방장 포함). 90일 snapshot 교집합 비용과 확정 직전 전원 15분 freshness 대기의 현실성을 기준으로 정했다.

## 확정한 구조

- 역할은 `owner` / `member` 두 가지다. 방장은 관리 권한을 갖지만 다른 멤버의 공개 수준과 캘린더 연결 상태는 볼 수 없다.
- 멤버십 상태는 `active` → `left` / `removed`이며 종단 상태다. 재가입은 새 멤버십 row를 만들고 공개 수준은 항상 `busyOnly`로 시작한다.
- 초대 상태는 `active` → `revoked` / `expired` / `exhausted`. 기본 만료 7일, `max_uses` 1~10, Party당 활성 초대 3개, token은 SHA-256 hash만 저장한다.
- 만료·무효·미존재 초대를 모두 `404 not_found`로 통일해 token 존재 여부를 구분할 수 없게 했다.
- 탈퇴·강퇴·해산은 단일 트랜잭션에서 `party_schedule_projections`와 `party_visibility_settings`를 삭제하고 떠난 사람과 남은 사람 양쪽에 tombstone을 보낸다. 개인 소유 `calendar_busy_facts`는 유지한다.
- 방장 계정 삭제 시 본인과 삭제 진행 중인 사용자를 제외한 활성 멤버 중 `joined_at` 오름차순 승계자에게 강제 위임하고, 후보가 없으면 해산한다. 비동기 삭제 작업이 사용자 상호작용을 기다릴 수 없으므로 결정적 규칙을 썼다.
- 멤버십 변화가 가능 시간 결과를 즉시 무효화하도록 응답에 `party_version`과 멤버십 version 해시를 포함시켰다.
- 오프라인 큐는 생성·이름 수정·탈퇴만 허용하고 초대 생성·수락·위임·강퇴·해산은 금지했다. 서버 전용 검사가 필요하거나 되돌릴 수 없는 작업이기 때문이다.

## 선행 설계와의 정합성

- `account_backend_design.md` §8의 `parties`, `party_memberships`, `party_invites` 스키마를 재정의하지 않고 필드만 추가했다.
- 멱등성, `expected_version`, 오류 코드 표, `sync_changes`/`outbox_jobs` 원자적 커밋 규칙을 그대로 따랐다.
- `calendar_privacy_sync_design.md` §8의 공개 수준 변경 규칙과 §9의 freshness gate에 멤버십 변화의 영향을 명시했다.

## 후속 설계로 넘긴 경계

이 설계는 `membership_ended`와 `party_disbanded` 신호 발행까지만 정의한다.

- 떠난 참여자가 있는 제안의 확정 조건과 상태 전이 → 설계 7
- 확정 이벤트의 캘린더 쓰기·취소 처리 → 설계 8
- 알림 문구, urgency, quiet hours → 설계 9
- 무료 Party 제한과 Premium → Post-MVP 결제 설계

## 산출물

- 상세 설계: `docs/party_membership_design.md`
- 구현 계획: `.omc/plans/party-membership-implementation.md`
- 백로그 갱신: `docs/feature_design_backlog.md`의 `확정 설계 4` 추가, 해결된 미확정 정책 2건 제거
- 진행 상황 갱신: `docs/progressing.md`

## 기존 구현과의 차이

`src/PlanTogether/Models.swift` 기준으로 멤버십·초대·Party 상태 개념이 전부 없다. 상세 표는 설계 §13에 있다.
특히 `AvailabilitySlot`의 `availableMemberCount`와 `totalMemberCount`는 "전원 공통 슬롯만 반환하고 인원수를 노출하지 않는다"는 확정 설계와 충돌하므로 구현 시 둘 다 제거해야 한다.
이번 작업에서는 Swift 코드를 변경하지 않았고 기존 코드는 그대로 빌드·실행 가능한 상태다.

## 부수 정리

- 구현 계획 디렉터리를 `.omx/plans/`에서 `.omc/plans/`로 통합했다. `.omx/`는 구버전 OMC 경로이고 현재 설정은 `.omc/`를 쓴다. 기존 계획 2건을 옮기고 rec 문서의 경로 표기를 맞췄다. `.omx/logs`, `.omx/state`는 그대로 남아 있어 추후 정리가 필요하다.
- `docs/implementation_plan.md`의 "현재 체크아웃 브랜치는 master" 표기가 실제 `main`과 다르다. 이번 작업 범위 밖이라 수정하지 않고 `progressing.md`에 기록했다.

## 검증

- 설계 작성 시 `account_backend_design.md` §8 불변식, §17 후속 결정 목록과 `calendar_privacy_sync_design.md` §8, §9를 대조했다.
- 별도 검토 에이전트가 재검증한 결과 `REVISE` 판정과 함께 Critical 4건, High 8건, Medium 11건, Low 5건을 지적했다. 모두 반영했다.

### 반영한 Critical

- `token_hash`가 `NOT NULL`인데 90일 정리 작업이 NULL을 쓰도록 정의돼 있었다. NULL 허용으로 바꾸고 전역 unique를 부분 unique로 교체했다.
- 소유권 불변식이 Party 상태를 한정하지 않아 해산(T7)이 항상 불변식을 위반했다. `status='active'`인 Party로 범위를 좁히고, disbanded Party의 `owner_membership_id`는 감사 참조로 정의했다.
- T1이 `owner_membership_id`를 "insert 후 update"로 채우도록 적혀 있었는데, PostgreSQL에서 `NOT NULL`은 지연 검증 대상이 아니라 실행 불가였다. 멤버십 UUID를 애플리케이션에서 미리 생성하고 FK만 `DEFERRABLE INITIALLY DEFERRED`로 두는 방식으로 단일화했다.
- 계정 삭제 시 단독 방장 경로가 T7 실행 후 다시 T5 종료를 지시해 종단 상태를 두 번 전이시켰다. T7 경로에서는 종료 단계를 건너뛰도록 분기를 명확히 했다.

### 반영한 주요 High

- `active` Party의 활성 멤버 1명 이상 등 불변식 3종이 T1·T7의 중간 상태에서 깨지므로, row 단위 CHECK가 아니라 지연 `CONSTRAINT TRIGGER`로 커밋 시점에 검증한다고 명시했다.
- 초대 수락(초대→Party)과 해산·위임(Party→초대)의 잠금 순서가 반대라 교착이 가능했다. `parties` → `party_memberships` → `party_invites` 전역 순서를 §5.5에 못 박고 T3 단계를 재배열했다.
- T4가 `party_invite` tombstone을 활성 멤버 전원에게 보내 "초대는 방장에게만"이라는 §9.2·§6 규칙을 위반했다. 수신자를 이전·새 방장으로 한정했다.
- §9.2의 entity type 목록이 T5·T7이 실제 발행하는 `party_schedule_projection`과 `proposal`을 빠뜨려 클라이언트 tombstone 계약이 비어 있었다. 목록에 추가했다.
- 가입 트랜잭션에 projection 생성 단계가 없어, 신규 멤버의 일정이 다음 snapshot 완료 전까지 다른 멤버에게 보이지 않았다. `§5.5.1 가입 시 projection 생성`을 신설했다.
- 초대 미리보기에 요청 제한이 없어 token 열거를 막을 방벽이 없었다. 분당 제한을 추가했다.
- "초대 수락은 이미 목표 상태면 200"과 "이미 멤버면 409"가 서로 모순이었다. 409로 통일했다.

### 2차 검토에서 반영한 것

1차 수정 자체가 만든 문제 2건이 High로 지적됐다.

- 가입 시 projection 생성(§5.5.1)을 T3에만 적용하고 **T1을 빠뜨렸다**. Party 생성 시점에는 그 Party가 없었으므로 생성자의 snapshot 완료 경로도 이미 지나간 뒤라, 첫 초대 수락 후 신규 멤버 화면에서 방장의 일정만 비어 보이게 된다. T1 step 6을 추가했다.
- §9.2에 `party_schedule_projection`을 "tombstone 전용"으로 적었는데 T3는 upsert를 발행한다. 문구대로 클라이언트 계약을 만들면 방금 고친 projection 전달이 사라진다. 발행 주체를 T1·T3 upsert / T5·T7 tombstone으로 분리해 명시했다.

Medium 4건도 반영했다.

- owner partial unique index는 지연할 수 없으므로 위임을 강등→승격 두 문으로 나눠야 한다는 제약을 명시했다. 지연 트리거는 `owner_membership_id` 3조건만 담당한다.
- T5 step 3이 T7로 넘어간 뒤 이후 단계를 수행하지 않는다는 명시가 없어 C4와 같은 이중 전이가 가능했다.
- `410 party_disbanded`를 반환하는 경로가 실제로는 없었다. 권한 검사가 먼저 실패해 `403`/`404`만 나가므로, Party row 잠금 직후 권한 검사보다 먼저 해산을 확인하도록 공통 단계를 추가했다.

### 3차 검토에서 반영한 것

2차 수정 중 해산 확인 단계를 필요한 범위보다 넓게 적용한 것이 지적됐다.

- "모든 Party mutation이 권한 검사보다 먼저 해산을 확인한다"고 써서, 아무 관계 없는 사용자가 UUID를 넣어 보는 것만으로 "존재했다가 해산된 Party"와 "없는 Party"를 구분할 수 있게 됐다. §6의 존재 은닉 규칙과 정면 충돌한다.
- 같은 규칙이 초대 수락 경로에도 걸려, "무효 token은 모두 404"와 "해산은 410"이 서로를 실패시키는 인수 조건 쌍을 만들었다.

멤버십 **이력** 조회를 먼저 두는 3단계 판정으로 바꿨다. 이력이 없으면 `404`, 이력이 있고 해산이면 `410`, 그 외에는 권한 검사로 진행한다. `/v1/invites/*` 경로는 이 규칙에서 제외하고 §4.2의 404 통일을 유지한다.
이 판정 순서가 "과거 멤버는 로컬 정리 시점을 알아야 한다"와 "비멤버에게 존재를 숨긴다"를 동시에 만족시킨다.
- 계획 문서의 entity type 개수와 승계자 규칙이 개정 전 서술로 남아 있었다.

### 설계 7에 남긴 제약

검토 과정에서 §5.6의 "비활성 멤버는 제안·응답 API에서 403"이 설계 7의 확정 조건 선택지를 이미 좁혔다는 점이 드러났다. `proposal_participants`가 고정 snapshot이므로 순수 "전원 수락"을 고르면 멤버가 한 명이라도 떠난 제안은 영구히 확정 불가가 된다.
문구를 "새 응답을 제출할 수 없다"로 완화하고, 설계 7이 반드시 선택해야 할 세 분기를 §5.6에 명시했다. §12에도 이 제약을 기록했다.

## 다음 작업

- Party별 공개 수준과 계산 반영 규칙 상세 설계 (설계 5)
