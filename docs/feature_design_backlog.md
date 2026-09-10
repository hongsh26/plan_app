# 그룹 일정 관리 앱 기능 설계 백로그

## 목적

이 문서는 필요한 기능을 빠짐없이 나열하고, 기능별 정책과 완료 기준을 하나씩 확정하기 위한 기준표다.

제품 범위는 다음 세 단계로 확정한다.

- `Internal Alpha`: 핵심 가치인 캘린더 연결 → Party → 공통 시간 → 제안 → 확정을 내부에서 검증하는 단계
- `Launch MVP`: 실제 사용자가 가입하고 여러 기기에서 사용할 수 있는 첫 출시 단계
- `Post-MVP`: 핵심 사용성 검증 이후 추가하거나 확장할 단계

`docs/group_calendar_mvp_design.md`의 전체 기능 목표는 장기 제품 백로그로 보존한다. 초기 출시는 아래 `Launch MVP` 범위로 한정하며, 나머지는 검증 이후 `Post-MVP`에서 구현한다.

## 현재 구현 기준

| 상태 | 범위 | 근거 |
|---|---|---|
| 구현 | SwiftUI 앱 골격과 Party/캘린더/설정 탭 | `src/PlanTogether/RootView.swift` |
| 부분 구현 | 메모리 기반 Party와 멤버, Party별 공개 수준 | `src/PlanTogether/Models.swift`, `src/PlanTogether/AppStore.swift` |
| 부분 구현 | Apple Calendar 권한 요청과 일정 읽기 | `src/PlanTogether/CalendarService.swift` |
| 부분 구현 | 전원 동기화 여부를 확인한 공통 가능 시간 계산 | `src/PlanTogether/AvailabilityCalculator.swift` |
| 부분 구현 | 가능 시간에서 제안 생성, 전원 수락 여부 계산 | `src/PlanTogether/AppStore.swift`, `src/PlanTogether/Models.swift` |
| 구현 | 공통 가능 시간 계산 단위 테스트 4개 | `src/PlanTogetherTests/AvailabilityCalculatorTests.swift` |

현재 구현은 데모 데이터와 메모리 상태를 사용한다. 실제 계정, 초대, 멤버별 응답, 서버 동기화, 영속화, 캘린더 쓰기, 알림, 결제는 아직 연결되지 않았다.

## 전체 기능 목록

| 순서 | 영역 | 필요한 기능 | 권장 단계 | 현재 상태 |
|---:|---|---|---|---|
| 0 | 제품 범위 | 단계별 컷라인, 지원 OS, Party/캘린더 제한 | Internal Alpha 선행 | 컷라인 확정 |
| 1 | 계정과 프로필 | Apple 로그인, 세션, 로그아웃, 탈퇴, 프로필 | Launch MVP | 미구현 |
| 2 | 데이터와 동기화 | 서버 저장, 기기 간 동기화, 오프라인 상태, 충돌 및 재시도 | Launch MVP | 미구현 |
| 3 | 캘린더 연결 | 권한 온보딩, 캘린더 선택, 일정 읽기, 권한 철회, 동기화 상태 | Alpha: Apple 읽기 / Launch: 쓰기·복구 | 설계 확정·부분 구현 |
| 4 | Party | 생성, 수정, 초대, 가입, 멤버 목록, 역할, 탈퇴, 강퇴, 해산 | Alpha 핵심 / Launch 완성 | 데모만 구현 |
| 5 | 공개와 개인정보 | Party별 공개 수준, 필드 마스킹, 계산 반영 규칙, 분석 로그 보호 | Internal Alpha | 부분 구현 |
| 6 | 가능 시간 | 검색 기간, 활동 가능 시간, 약속 길이, 슬롯, 시간대, 전체/부분 일치 | Internal Alpha | 핵심 계산만 부분 구현 |
| 7 | 약속 제안 | 제목·시간·장소·참여자, 응답 기한, 수정, 취소, 재제안 | Internal Alpha | 생성 데모만 구현 |
| 8 | 응답과 확정 | 수락·거절·미정, 응답 변경, 확정 조건, 충돌 재검사, 상태 전이 | Internal Alpha | 모델 계산만 부분 구현 |
| 9 | 외부 캘린더 반영 | 확정 일정 생성, 사용자별 반영 상태, 중복 방지, 변경·삭제·재시도 | Launch MVP | 미구현 |
| 10 | 알림 | 초대, 제안, 응답, 확정, 변경, 취소, 알림 설정과 중복 방지 | Launch MVP | 미구현 |
| 11 | 요금제와 결제 | 무료 제한, Premium 권한, 구매, 복원, 만료·환불 반영 | Post-MVP | 미구현 |
| 12 | 운영과 품질 | 접근성, 개인정보 삭제, 분석 지표, 오류 관측, 지원/복구 안내 | Launch MVP | 미구현 |
| 13 | 확장 기능 | Google/이메일 로그인, Google Calendar, 부분 가능 시간, 반복 약속, 고급 알림, Widget, 장소/AI 추천 | Post-MVP | 미구현 |

## 기능별 상세 설계 형식

각 영역은 아래 항목을 모두 채워야 설계 완료로 본다.

1. 목표와 사용자 가치
2. 포함 범위와 제외 범위
3. 사용자 흐름과 화면 상태
4. 입력값과 검증 규칙
5. 데이터 모델과 상태 전이
6. 역할별 권한
7. 개인정보 노출 규칙
8. 정상·빈 상태·로딩·오류·오프라인 상태
9. 외부 시스템 연동과 재시도/중복 방지
10. 테스트 가능한 인수 조건
11. 분석 지표와 민감정보 제외 기준
12. 미결정 사항과 후속 범위

## 상세 설계 진행 순서

의존성을 기준으로 다음 순서로 확정한다.

1. 출시 범위와 단계별 컷라인
2. 계정·서버·기기 간 데이터 소유권
3. 캘린더 권한·읽기·동기화
4. Party 생성·초대·역할·탈퇴
5. Party별 공개 수준과 계산 반영 규칙
6. 공통 가능 시간 검색 조건과 결과 표시
7. 제안·응답·확정 상태 전이
8. 확정 일정의 외부 캘린더 쓰기·변경·취소
9. 알림 종류와 전송 규칙
10. 무료 제한·Premium·결제
11. 계정 삭제·분석·오류 관측·출시 검증

앞 단계의 결정이 다음 단계의 데이터 모델이나 상태 전이를 바꾸므로, 순서대로 설계한다.

### 현재 실행 컷라인

- 이 절의 번호는 위 `전체 기능 목록`의 기능 번호가 아니라 `상세 설계 진행 순서` 번호를 뜻한다.
- 상세 설계 8을 완료했으며, 이어서 상세 설계 9를 확정한다.
- 상세 설계 9(알림)가 확정되기 전에는 새 기능 구현에 착수하지 않는다.
- 상세 설계 9까지 확정한 뒤 상세 설계 1~9에서 확정한 기능을 의존성 순서에 따라 단계별로 구현한다.
- 상세 설계 10(무료 제한·Premium·결제)과 11(계정 삭제·분석·오류 관측·출시 검증)은 상세 설계 1~9의 기능 구현 진행 후 다시 우선순위를 정한다.

## 확정 설계 1: 출시 범위 컷라인

### Internal Alpha

- 로컬 또는 테스트 사용자
- Apple Calendar 읽기
- 데모 또는 테스트 Party
- Busy Only 공개
- 전원 공통 시간 계산
- 약속 제안, 전원 수락, 앱 내부 확정

### Launch MVP

- Apple 로그인과 세션 유지
- 서버 저장과 기기 간 동기화
- 실제 Party 생성·초대·가입·탈퇴
- Party별 공개 수준
- 공통 시간 검색과 약속 제안·응답·확정
- Apple Calendar 일정 쓰기·변경·취소와 실패 복구
- 초대·제안·확정·변경·취소에 필요한 푸시 알림
- 계정 및 사용자 데이터 삭제

### Post-MVP

- Google/이메일 로그인과 Google Calendar
- 부분 가능 시간
- 앱 내 반복 약속
- 무료 제한, Premium과 인앱 결제
- 고급 알림과 Widget
- 장소 및 AI 추천

### Launch MVP 필수 사용자 시나리오

> 사용자가 Apple로 로그인하고 캘린더를 연결한 뒤 Party에 구성원을 초대하여 공개 범위를 설정하고, 모두 가능한 시간을 찾아 약속을 제안·확정한 다음 각자의 Apple Calendar에 반영하고 변경·취소 알림까지 받을 수 있다.

### 완료 조건

- [x] 모든 기능이 세 단계 중 하나에 배치됐다.
- [x] Launch MVP의 필수 사용자 시나리오를 한 문장으로 정의했다.
- [x] 첫 출시를 막는 기능과 출시 후 추가 가능한 기능을 구분했다.
- [x] 원본 제품 목표는 장기 백로그로 보존했다.

## 아직 확정되지 않은 핵심 정책

- 무료 Party 제한이 생성 Party만인지 참여 Party 전체인지 (Post-MVP 결제 설계)

`확정 설계 4`에서 초대 만료·재사용 규칙, 비회원 초대 여부, 방장 탈퇴 정책, 멤버 상한을 확정했다.
`확정 설계 4` §4.3의 시스템 한도(활성 멤버 10명, 소유 Party 10개, 참여 Party 20개)는 남용 방지 한도이며 요금제 제한과 별개다. 무료/Premium 구분은 이 한도 안에서 정한다.
`확정 설계 5~6`에서 공개 수준별 계산·비노출 규칙과 공통 시간의 활동 시간·검색·정렬 정책을 확정했다.
`확정 설계 7`에서 고정 참여자 전원 수락과 freshness·충돌 재검사, 떠난 참여자 발생 시 재제안 정책을 확정했다.
`확정 설계 8`에서 앱 내부 확정과 외부 반영의 분리, 확정 일정 취소·재일정, 반영 상태의 본인 전용 노출을 확정했다. 이로써 캘린더 쓰기 실패 표시 정책은 미결 항목에서 해소됐다.

## 확정 설계 2: 계정·서버·데이터 소유권

### 확정된 원칙

- 초기 개발 속도보다 인프라 통제와 장기 확장성을 우선한다.
- 앱이 특정 BaaS의 데이터 모델이나 권한 규칙에 직접 종속되지 않도록 자체 API 경계를 둔다.
- 서버가 계정, Party, 멤버십, 초대, 공개 정책, 제안·응답·확정 상태의 기준 데이터(source of truth)를 소유한다.
- 개인 캘린더 원본의 상세 데이터는 필요한 범위만 처리하고, 서버 저장 범위는 개인정보 설계에서 별도로 최소화한다.
- 외부 캘린더 반영과 푸시 알림은 요청 처리와 분리된 재시도 가능한 작업으로 설계한다.

### 아키텍처 선택 기준

1. PostgreSQL과 명시적 마이그레이션을 통한 데이터 소유권
2. 상태 전이와 권한 검증을 서버에서 일관되게 강제할 수 있는 구조
3. API 서버와 비동기 작업자의 독립 확장
4. Apple 로그인 검증, APNs, 향후 Google/결제 어댑터의 교체 가능성
5. 자체 호스팅과 클라우드 이전이 가능한 표준 구성 요소
6. 관측성, 백업, 복구, 보안 패치의 운영 가능성

### 확정 기술 구성

- Go 모듈러 모놀리스와 PostgreSQL을 사용한다.
- REST/JSON과 OpenAPI로 iOS-서버 계약을 관리한다.
- 사용자별 cursor 증분 동기화와 APNs invalidation을 사용한다.
- PostgreSQL outbox/job으로 시작하고 캐시는 필요할 때만 추가한다.
- iOS와 서버는 현재 단일 저장소에서 함께 관리한다.
- 전체 상세 설계는 `docs/account_backend_design.md`를 기준으로 한다.

### 확정안: Go 자체 API

- API 서버: Go 표준 HTTP 서버와 얇은 라우터
- 기준 데이터: PostgreSQL과 명시적 스키마 마이그레이션
- 데이터 접근: PostgreSQL 전용 드라이버와 명시적 SQL 우선
- 비동기 작업: PostgreSQL 트랜잭션과 함께 기록되는 outbox/job 테이블로 시작
- 캐시: 초기 핵심 경로에서는 사용하지 않고, 요청 제한이나 짧은 TTL 캐시가 필요할 때 Valkey를 추가
- 배포 단위: 같은 코드베이스와 컨테이너 이미지에서 `api`, `worker`, `scheduler` 프로세스를 분리
- 관리형 서비스 경계: PostgreSQL의 HA·백업과 컨테이너 실행 환경은 관리형을 허용하되, 도메인 API와 데이터 모델은 자체 소유

#### 선택 이유

- 작은 런타임과 단순한 배포 단위로 API와 작업자를 독립 확장하기 쉽다.
- PostgreSQL 트랜잭션 안에서 상태 전이와 비동기 작업 생성을 함께 보장하기 좋다.
- 특정 클라우드나 BaaS의 SDK 없이 표준 컨테이너와 PostgreSQL을 중심으로 이전할 수 있다.
- Apple 로그인, APNs, 향후 Google Calendar와 결제를 외부 어댑터로 분리하기 쉽다.

#### 초기부터 넣지 않는 것

- Kubernetes
- 마이크로서비스 분리
- Kafka 같은 별도 메시지 브로커
- 핵심 데이터의 캐시 의존
- 자체 PostgreSQL HA 운영

이 항목들은 규모나 장애 요구가 실제로 생기기 전에는 통제력을 높이기보다 운영 실패 지점을 늘릴 가능성이 크다.

#### 대안 위치

- Kotlin/Spring: 대규모 조직과 복잡한 엔터프라이즈 통합이 우선이면 강하지만 초기 운영과 코드 표면이 더 크다.
- TypeScript/NestJS: 개발 속도가 최우선이면 강하지만 현재의 통제·운영 단순성 우선순위에서는 후순위다.
- Swift/Vapor: iOS와 언어를 통일할 수 있으나 서버 생태계와 작업 큐 선택 폭 때문에 채택하지 않는 편이 낫다.

구현 시점에는 Go와 PostgreSQL의 지원 중인 안정 버전을 공식 문서로 다시 확인해 고정한다.

## 확정 설계 3: Apple Calendar 권한·동기화·공개

- 사용자가 명시적으로 선택한 Apple Calendar만 읽는다.
- `hidden`은 다른 멤버에게 projection을 만들지 않지만 private Busy fact로 계산과 충돌 검사에 반영한다.
- Launch MVP는 개인별 가능 여부가 아닌 전원 공통 슬롯만 반환한다.
- rolling 90일 snapshot과 6시간 검색 freshness, 15분 확정 freshness를 사용한다.
- 실제 EventKit 쓰기는 지정 iOS 기기가 수행하고 서버는 명령과 상태를 관리한다.
- 상세 계약과 인수 조건은 `docs/calendar_privacy_sync_design.md`를 기준으로 한다.

## 확정 설계 4: Party 생성·초대·역할·탈퇴

### 확정된 제품 정책

- 방장은 다른 활성 멤버에게 소유권을 위임해야만 탈퇴할 수 있다. 단독 멤버일 때만 탈퇴가 곧 해산이다.
- 초대는 재사용 가능한 Universal Link로 하고, 수락하려면 Apple 로그인이 완료된 계정이어야 한다. 비회원 초대는 Post-MVP다.
- Party 활성 멤버 상한은 방장을 포함해 10명이다.

### 확정된 구조

- 역할은 `owner`와 `member` 두 가지다. 방장은 다른 멤버의 공개 수준을 보거나 바꿀 수 없다.
- 멤버십 상태는 `active` → `left` / `removed`이며 종단 상태다. 재가입은 기존 row를 되살리지 않고 새 멤버십 row를 만든다.
- 초대 상태는 `active` → `revoked` / `expired` / `exhausted`다. 기본 만료 7일, `max_uses` 최대 10, Party당 활성 초대 3개다.
- 초대 token은 SHA-256 hash만 저장하고 만료·무효·미존재를 모두 `404 not_found`로 통일해 존재 여부를 숨긴다.
- `active` Party에 `role='owner' AND status='active'` 멤버십은 항상 정확히 1개이며 partial unique index로 강제한다. `disbanded` Party에는 0개다.
- 탈퇴·강퇴·해산은 같은 트랜잭션에서 해당 Party의 `party_schedule_projections`와 공개 설정을 삭제하고 떠난 사람과 남은 사람 양쪽에 tombstone을 보낸다. 개인 소유 `calendar_busy_facts`는 유지한다.
- 방장이 계정을 삭제하면 본인과 삭제 진행 중인 사용자를 제외한 활성 멤버 중 `joined_at` 오름차순으로 결정된 승계자에게 강제 위임하고, 후보가 없으면 해산한다.
- 멤버십 변화는 가능 시간 결과를 즉시 무효화한다. 응답에 포함된 멤버십 해시가 다르면 클라이언트는 캐시를 재사용하지 않는다.

### 후속 설계로 넘긴 경계

- 떠난 참여자가 있는 제안의 확정 조건과 상태 전이는 상세 설계 7에서 정한다. 이 설계는 `membership_ended`, `party_disbanded` 신호 발행까지만 정의한다.
- 확정된 이벤트의 캘린더 쓰기·취소 처리는 상세 설계 8에서 정한다.
- 알림 문구, urgency, quiet hours는 상세 설계 9에서 정한다.

### 완료 조건

- [x] 상세 설계 형식 12개 항목을 모두 채웠다.
- [x] `account_backend_design.md` §8의 고정 스키마를 재정의하지 않고 확장했다.
- [x] 탈퇴·강퇴·해산의 projection 삭제와 tombstone 전파를 트랜잭션 단위로 정의했다.
- [x] 계정 삭제 시 소유권 승계 규칙을 결정적으로 정의했다.
- [x] 후속 설계와의 경계를 명시했다.

상세 설계는 `docs/party_membership_design.md`, 구현 계획은 `.omc/plans/party-membership-implementation.md`를 기준으로 한다.

## 확정 설계 5: Party별 공개 수준과 계산 반영 규칙

### 확정된 제품 정책

- 공개 수준은 사용자·Party 단위로 `details`, `busyOnly`, `hidden` 중 하나를 선택하며 기본값은 항상 `busyOnly`다.
- `details`는 시간과 제목을 공유하고, 장소는 기본이 꺼진 별도 `share_location` 동의가 있어야 공유한다.
- `hidden`은 Party projection을 만들지 않지만 private Busy fact는 가능 시간과 충돌 계산에 동일하게 반영한다.
- 공개 설정값과 변경 이력은 본인만 조회한다. 방장도 다른 멤버의 공개 수준을 볼 수 없다.
- Launch MVP 결과는 전원 공통 슬롯만 반환하며 가능 인원, 불가능 원인과 소유자를 노출하지 않는다.

### 확정된 구조

- 계산 엔진은 `calendar_busy_facts`, Party 일정 화면은 `party_schedule_projections`를 사용해 입력 경계를 분리한다.
- `busyOnly` projection에는 상세 ciphertext가 없고, `hidden` projection row는 존재할 수 없다.
- 공개 수준 하향, projection 삭제와 sync tombstone은 같은 transaction에서 커밋한다.
- snapshot 완료와 설정 변경의 경쟁은 source generation과 setting version guard로 stale projection 재생성을 막는다.
- details 자료가 아직 없으면 `pending_details_refresh`로 표시하고 busyOnly로 안전 강등한 뒤 최신 snapshot에서만 승격한다.
- 공개 설정 sync는 본인에게만, 허용된 projection sync는 활성 Party 멤버에게만 전달한다.

### 완료 조건

- [x] 세 공개 수준의 표시 필드와 계산 반영 규칙을 확정했다.
- [x] 장소 공유를 별도 명시적 동의로 정의했다.
- [x] API·sync·로컬 캐시의 허용 목록과 tombstone 규칙을 정의했다.
- [x] snapshot·멤버십·설정 변경 경쟁의 stale write 차단 규칙을 정의했다.
- [x] 로그·분석·APNs 민감정보 제외 기준과 검증 계획을 작성했다.

상세 설계는 `docs/party_visibility_design.md`, 구현 계획은 `.omc/plans/party-visibility-implementation.md`를 기준으로 한다.

## 확정 설계 6: 공통 가능 시간 검색 조건과 결과 표시

### 확정된 제품 정책

- 사용자별 주간 활동 시간은 명시적으로 저장해야 하며, 미설정 상태를 하루 전체 가능으로 해석하지 않는다.
- UI는 매일 09:00~22:00을 제안하지만 사용자가 저장하기 전에는 효력이 없다.
- 기본 검색은 오늘 포함 14일, 60분 약속, 최대 20개 결과다.
- 검색 범위는 최대 31일, 약속 길이는 30분~8시간의 30분 배수다.
- 후보는 요청 표시 시간대의 00/30분 경계에서 만들고 하루 최대 3개를 시작 시각 순으로 반환한다.
- 전원 공통 슬롯만 제공하고 부분 가능 인원, 제외 원인과 멤버별 준비 상태는 노출하지 않는다.

### 확정된 구조

- 각 멤버의 활동 시간에서 private Busy facts를 뺀 뒤 모든 활성 멤버의 교집합을 계산한다.
- REPEATABLE READ snapshot에서 Party·membership·activity·calendar generation과 최소 freshness deadline을 고정하고,
  모든 멤버의 active generation window UTC 교집합이 요청 전체를 덮는지 확인한다.
- 응답의 opaque `calculationVersion`이 현재 tuple과 다르거나 `validUntil`이 지나면 제안 진입 전 재검색한다.
- 활동 시간 미설정과 calendar freshness 실패는 정상 빈 결과와 구분되는 aggregate pending 오류다.
- 캐시는 version tuple과 검색 파라미터를 key로 최대 5분만 사용할 수 있으며 correctness가 캐시에 의존하지 않는다.

### 완료 조건

- [x] 활동 시간 초기 상태·주간 규칙·시간대와 DST 처리 규칙을 확정했다.
- [x] 검색 기본값·최대 범위·약속 길이·30분 grid를 확정했다.
- [x] 날짜당/전체 결과 제한과 안정 정렬 규칙을 정의했다.
- [x] 준비 미완료, 정상 빈 결과, stale 결과의 API 상태를 분리했다.
- [x] 계산 version·캐시 무효화·개인정보 응답 제한과 검증 계획을 작성했다.

상세 설계는 `docs/availability_search_design.md`, 구현 계획은 `.omc/plans/availability-search-implementation.md`를 기준으로 한다.

## 확정 설계 7: 제안·응답·확정 상태 전이

### 확정된 제품 정책

- 제안 참여자는 생성 시점의 활성 Party 멤버 전원으로 고정하며 생성자는 자동 수락한다.
- 확정 조건은 고정 참여자 전원의 활성 멤버십 유지·전원 수락·기한 미도래·15분 calendar freshness·private Busy와 앱 내부 reservation 충돌 없음이다.
- 미확정 제안의 참여자가 떠나면 남은 인원만으로 확정하지 않고 `reproposal_required`로 종료해 현재 멤버로 재제안한다.
- 제안 제목·장소·시간·기한과 참여자 snapshot은 생성 후 수정하지 않는다. 변경은 새 제안을 만들고 기존 제안을 `superseded`로 닫는다.
- 거절과 미정 응답은 기한 전 변경할 수 있으며 즉시 제안을 종료하지 않는다.
- 확정 직전 충돌은 원인 사용자·일정·hidden 여부를 노출하지 않고 `conflict_detected`로 종료한다.

### 확정된 구조

- proposal 상태는 `open`, `awaiting_calendar_refresh`와 7개 종단 상태(`confirmed`, `conflict_detected`, `reproposal_required`, `party_disbanded`, `expired`, `cancelled`, `superseded`)로 관리한다.
- 전원 수락 시 15분 freshness가 부족하면 최대 2분 refresh를 요청하며 자동 확정하지 않는다.
- confirmed event와 사용자별 server-side reservation을 외부 EventKit 쓰기 전에 생성해 서로 다른 Party의 동시 확정 겹침도 차단한다.
- membership 종료·Party 해산과 관련 proposal 전이는 같은 transaction에서 처리해 확정 가능한 틈을 만들지 않는다.
- proposal/response/confirmed event 상태, sync와 domain outbox는 원자적으로 커밋한다.
- 외부 캘린더 반영과 확정 일정 변경·취소는 상세 설계 8, 실제 알림 전송 정책은 상세 설계 9가 소유한다.

### 완료 조건

- [x] 제안 참여자 snapshot과 전원 수락 확정 조건을 확정했다.
- [x] 떠난 참여자·신규 가입자·해산 Party의 처리 규칙을 확정했다.
- [x] freshness 대기, 충돌 재검사와 동시 확정 방지 계약을 정의했다.
- [x] 취소·만료·재제안과 불변 본문 정책을 정의했다.
- [x] API·권한·오프라인·sync·outbox·privacy와 테스트 기준을 작성했다.

상세 설계는 `docs/proposal_lifecycle_design.md`, 구현 계획은 `.omc/plans/proposal-lifecycle-implementation.md`를 기준으로 한다.

## 확정 설계 8: 확정 일정의 외부 캘린더 쓰기·변경·취소

### 확정된 제품 정책

- `confirmed_events`가 앱 내부 기준 데이터이며 외부 캘린더 반영 성공 여부와 완전히 분리된다.
- 확정 일정의 본문은 수정하지 않는다. 변경은 취소 + 재제안이며, 두 작업을 한 transaction에서 처리하는 재일정 흐름을 제공한다.
- 외부 캘린더 반영 상태는 본인 전용 리소스다. 다른 멤버에게는 집계 형태로도 노출하지 않는다.
- 명시적 약속 취소만 서버가 delete command를 발급한다. 탈퇴·강퇴·해산은 이미 반영된 외부 일정을 자동 삭제하지 않고 본인 기기의 로컬 정리 제안으로 처리한다.
- EventKit 쓰기는 지정 writer device만 수행하며 서버는 명령 상태만 소유한다.
- 취소 권한은 원 제안의 생성자와 Party 방장에게 있다.

### 확정된 구조

- 외부 반영 세대는 사용자별 `calendar_write_commands.revision`이 담당하고, `confirmed_events.revision`은 본문 세대로서 본문 불변 원칙에 따라 1로 고정된다.
- command 상태 기계는 상세 설계 3 §10.1의 `pending_device → claimed → succeeded / retryable_failed / user_action_required / cancelled`를 그대로 사용한다.
- 활성 command는 `(confirmed_event, user)`당 하나이며 새 revision 발급은 기존 활성 command 종료와 같은 transaction에서 일어난다.
- 취소는 참여자 reservation을 해제해 같은 시간의 재확정을 가능하게 하고, 직전 create 세대 상태에 따라 delete 발급 여부를 결정한다.
- claim은 90초 lease와 `SKIP LOCKED`, 결과 보고는 `lease_token` 기반 멱등 처리로 다기기 경쟁을 막는다.
- membership 종료·해산·계정 삭제 handler는 관계 종료와 command 종료를 같은 transaction에서 끝낸다.
- domain outbox type 4종만 확정하고 문구·수신자·urgency는 상세 설계 9가 소유한다.

### 완료 조건

- [x] 앱 내부 확정과 사용자별 외부 반영을 분리하는 기준을 확정했다.
- [x] confirmed event의 취소·재일정과 revision 의미를 선행 설계와 정합하게 확정했다.
- [x] 떠난 사용자와 해산 Party의 미완료 command·기존 외부 일정 처리를 확정했다.
- [x] 중복 방지, lease, 재시도, 조치 필요 복구 경로를 정의했다.
- [x] API·권한·오프라인·sync·outbox·privacy와 테스트 기준을 작성했다.
- [x] 상세 설계 형식 12개 항목을 모두 채웠다.

상세 설계는 `docs/calendar_write_design.md`, 구현 계획은 `.omc/plans/calendar-write-implementation.md`를 기준으로 한다.
