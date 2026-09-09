# Party 생성·초대·역할·탈퇴 상세 설계

## 0. 설계 상태

- 상태: 확정
- 대상: Launch MVP
- 순서: `docs/feature_design_backlog.md`의 상세 설계 진행 순서 4번
- 선행 설계: `docs/account_backend_design.md`, `docs/calendar_privacy_sync_design.md`
- 후속 설계: 5(공개 수준 세부), 6(가능 시간 검색), 7(제안·응답·확정), 8(캘린더 쓰기), 9(알림)

이 설계는 Party의 생성, 수정, 초대, 가입, 역할, 소유권 이전, 탈퇴, 강퇴, 해산과 계정 삭제 연동을 확정한다.
선행 설계에서 이미 고정된 스키마(`account_backend_design.md` §8), 멱등성·버전·오류 계약(§6), projection 규칙
(`calendar_privacy_sync_design.md` §8)과 freshness gate(§9)는 재정의하지 않고 확장한다.

### 이번 설계에서 확정한 제품 정책

| 항목 | 결정 | 근거 |
|---|---|---|
| 방장 탈퇴 | 위임 후 탈퇴 강제. 단독 멤버일 때만 해산 허용 | `parties.owner_membership_id` NOT NULL 불변식 유지, 진행 중 제안·확정 일정 보존 |
| 초대 방식 | 재사용 가능한 링크 + 수락 시 앱 내 계정 필수 | 서버에 pre-account 상태를 만들지 않아 스키마와 삭제 경로가 단순함 |
| 멤버 상한 | 활성 멤버 10명(방장 포함) | 90일 snapshot 교집합 비용과 확정 직전 전원 15분 freshness 대기의 현실성 |

---

## 1. 목표와 사용자 가치

1. 사용자가 약속을 잡을 실제 그룹을 앱 안에서 만들고 유지할 수 있다.
2. 초대 링크 하나로 여러 명을 모을 수 있고, 링크가 유출돼도 피해 범위가 제한된다.
3. 그룹의 소유권이 항상 한 명에게 명확히 존재해 관리 작업(이름 변경, 강퇴, 해산)의 책임자가 분명하다.
4. 떠난 사람의 일정 정보가 그룹에 남지 않고, 남은 사람의 그룹은 계속 동작한다.
5. 모든 멤버십 변화가 서버 기준 데이터로 관리되어 여러 기기에서 같은 상태로 보인다.

### 비목표

- 그룹 채팅, 공지, 프로필 이미지 같은 커뮤니티 기능
- 조직/워크스페이스 계층이나 하위 그룹
- 공개 검색 가능한 Party
- 요금제에 따른 Party 개수 제한 (Post-MVP 결제 설계)

---

## 2. 포함 범위와 제외 범위

### 포함

- Party 생성, 이름 수정, 조회
- 초대 링크 생성·조회·무효화, 만료·사용 횟수 관리
- 초대 수락을 통한 가입
- `owner` / `member` 두 역할과 역할별 권한
- 소유권 위임
- 자발적 탈퇴, 방장의 강퇴, 방장의 해산
- 탈퇴·강퇴·해산 시 projection 삭제와 tombstone 전파
- 재가입 규칙
- 계정 삭제 시 멤버십·소유권 처리
- 남용 방지 한도와 요청 제한

### 제외

- 공개 수준의 세부 의미와 필드 마스킹 규칙 → 설계 5, `calendar_privacy_sync_design.md` §3, §8
- 가능 시간 검색 조건과 결과 표시 → 설계 6
- 멤버 변화가 진행 중인 제안의 확정 조건에 미치는 영향 → 설계 7
- 확정 일정의 캘린더 쓰기·취소 → 설계 8, `calendar_privacy_sync_design.md` §10
- 초대·탈퇴·강퇴 푸시 알림의 문구, urgency, quiet hours → 설계 9
- 무료 Party 제한과 Premium → Post-MVP 결제 설계
- 비회원 초대, 이메일/전화번호 초대, QR 초대 → Post-MVP

---

## 3. 사용자 흐름과 화면 상태

### 3.1 Party 생성

1. Party 탭에서 `새 Party`를 선택한다.
2. 이름을 입력한다. 이름만 필수 입력이다.
3. 생성 요청이 성공하면 생성자가 `owner`인 활성 멤버십 1건과 함께 Party가 만들어진다.
4. 생성 직후 화면은 멤버 1명 상태의 Party 상세이며 `초대 링크 만들기`를 우선 제시한다.
5. 생성자의 해당 Party 공개 수준은 `busyOnly`로 초기화한다.

### 3.2 초대와 가입

```text
[방장] 초대 링크 생성 ─▶ 링크 공유(시스템 공유 시트)
                              │
                       [수신자] 링크 열기
                              ├─ 앱 미설치 ─▶ App Store ─▶ 설치 후 링크 재개
                              ├─ 미로그인 ─▶ Apple 로그인 ─▶ 초대 미리보기
                              └─ 로그인됨 ─▶ 초대 미리보기
                                              │
                              ┌───────────────┼───────────────┐
                          [수락]           [거절]         [불가]
                              │               │               │
                     활성 멤버십 생성      화면 종료   만료/무효/정원초과 안내
                     공개 수준 busyOnly
                     캘린더 미연결이면 연결 CTA
```

- 초대 링크는 Universal Link `https://<서비스 도메인>/i/{token}` 형식이다.
- 미리보기는 Party 이름, 활성 멤버 수, 초대자 표시 이름만 보여준다. 멤버 명단과 일정은 수락 전에 보여주지 않는다.
- 수락은 반드시 로그인된 계정으로만 가능하다. 로그인 전에는 서버에 어떤 멤버십도 만들지 않는다.
- 이미 활성 멤버인 사용자가 링크를 열면 수락 대신 해당 Party 상세로 이동한다.

### 3.3 멤버 목록과 관리

- 멤버 목록은 표시 이름, 역할 배지(`방장`), 본인 표시, 캘린더 연결 여부의 본인 항목만 보여준다.
- 방장에게만 각 멤버의 `내보내기`와 상단의 `소유권 넘기기`, `Party 해산`이 보인다.
- 일반 멤버에게는 `Party 나가기`만 보인다.

### 3.4 소유권 위임

1. 방장이 `소유권 넘기기`를 선택한다.
2. 활성 멤버 중 본인을 제외한 목록에서 한 명을 고른다.
3. 확인 다이얼로그에 "넘긴 뒤에는 되돌릴 수 없고 관리 권한을 잃는다"를 명시한다.
4. 성공하면 즉시 화면의 관리 항목이 사라진다.

### 3.5 탈퇴

- 일반 멤버: 확인 후 즉시 탈퇴. "내 일정 정보가 이 Party에서 삭제된다"를 안내한다.
- 방장이면서 다른 활성 멤버가 있는 경우: 나가기를 누르면 소유권 위임 화면으로 유도하고, 위임 없이 탈퇴할 수 없음을 설명한다.
- 방장이면서 단독 멤버인 경우: 나가기는 곧 해산이며 "Party가 삭제된다"로 문구를 바꿔 확인받는다.

### 3.6 강퇴와 해산

- 강퇴: 방장이 대상 멤버를 선택하고 확인한다. 대상자에게는 알림이 가고 목록에서 Party가 사라진다.
- 해산: 방장이 Party 이름을 직접 입력해야 실행되는 파괴적 확인을 사용한다.

---

## 4. 입력값과 검증 규칙

### 4.1 Party 이름

| 규칙 | 값 |
|---|---|
| 정규화 | 유니코드 NFC, 앞뒤 공백 제거, 연속 공백 1칸으로 축약 |
| 길이 | 정규화 후 1자 이상 40자 이하 (grapheme cluster 기준) |
| 금지 | 제어문자, 줄바꿈, 양방향 제어문자(BiDi override) |
| 중복 | 허용. 같은 사용자가 같은 이름의 Party를 여러 개 가질 수 있다 |

위반 시 `400 invalid_request`.

### 4.2 초대

| 항목 | 값 | 비고 |
|---|---|---|
| token | 256-bit CSPRNG 난수, base64url 인코딩 | 서버에는 SHA-256 hash만 저장 |
| 기본 만료 | 생성 후 7일 | 요청으로 1시간~30일 지정 가능 |
| 기본 `max_uses` | `max(1, min(남은 정원, 10))` | 요청으로 1~10 지정 가능. 남은 정원이 0이면 초대 생성 자체를 `409 party_full`로 거부한다 |
| Party당 활성 초대 | 최대 3개 | 초과 시 `409 party_invite_limit_reached` |
| 생성 제한 | Party당 시간당 10회 | 초과 시 `429 rate_limited` |
| 수락 시도 제한 | 사용자당 분당 5회, IP당 분당 30회 | 초과 시 `429 rate_limited` |
| 미리보기 조회 제한 | 사용자당 분당 10회, IP당 분당 30회 | 초과 시 `429 rate_limited`. 미리보기도 token 유효성을 알려주는 검증 경로이므로 제한하지 않으면 §4.2의 404 통일이 무력화된다 |

- token 원문은 응답 본문에만 1회 반환하고 서버 로그, audit, 분석 이벤트, 푸시 payload에 남기지 않는다.
- 잘못된 token, 만료된 token, 무효화된 token은 모두 `404 not_found`로 응답해 존재 여부를 구분할 수 없게 한다.
- 정원 초과와 이미 멤버인 경우만 별도 코드를 반환한다. 이 두 경우는 token이 유효함이 이미 전제되므로 추가 정보 유출이 아니다.

### 4.3 남용 방지 한도

| 대상 | 한도 | 초과 시 |
|---|---|---|
| Party 활성 멤버 | 10명 (방장 포함) | `409 party_full` |
| 사용자가 소유한 활성 Party | 10개 | `409 party_owned_limit_reached` |
| 사용자가 참여한 활성 Party | 20개 | `409 party_joined_limit_reached` |

이 값은 요금제 제한이 아니라 계산 비용과 남용 방지를 위한 시스템 한도다. 무료/Premium 구분은 Post-MVP 결제 설계에서 별도로 정하며 이 한도를 넘어설 수 없다.

### 4.4 공통 요청 규칙

`account_backend_design.md` §6을 그대로 따른다.

- 모든 mutation은 `Idempotency-Key`를 요구한다.
- Party 이름 수정, 소유권 위임, 강퇴, 해산은 `If-Match` 또는 `expected_version`을 요구한다.
- 탈퇴는 대상이 이미 비활성 멤버십이면 현재 resource와 `200`을 반환하는 멱등 동작으로 처리한다.
- 초대 수락은 이미 활성 멤버면 `409 already_member`를 반환하고 클라이언트가 Party 상세로 이동한다. 같은 `Idempotency-Key`의 재전송만 저장된 결과를 재사용한다.
- 모든 Party mutation의 `expected_version`은 `parties.version`을 의미한다 (§5.5).

---

## 5. 데이터 모델과 상태 전이

### 5.1 스키마

`account_backend_design.md` §8에 이미 고정된 세 테이블을 확장한다. 기존 필드와 제약은 변경하지 않는다.

#### `parties`

| 필드 | 타입 | 비고 |
|---|---|---|
| `id` | uuid PK | 기존 |
| `name` | text NOT NULL | 기존, §4.1 검증 |
| `status` | enum(`active`,`disbanded`) NOT NULL | 기존 |
| `version` | bigint NOT NULL | 기존 |
| `owner_membership_id` | uuid NOT NULL | 기존. `party_memberships(id)` 참조. FK를 `DEFERRABLE INITIALLY DEFERRED`로 선언 |
| `member_limit` | smallint NOT NULL DEFAULT 10 | 추가. `CHECK (member_limit BETWEEN 1 AND 10)` |
| `created_by_user_id` | uuid NOT NULL | 추가. 감사용, 소유권과 무관 |
| `disbanded_at` | timestamptz NULL | 추가 |

#### `party_memberships`

| 필드 | 타입 | 비고 |
|---|---|---|
| `id` | uuid PK | |
| `party_id` | uuid NOT NULL | |
| `user_id` | uuid NOT NULL | |
| `role` | enum(`owner`,`member`) NOT NULL | 기존 |
| `status` | enum(`active`,`left`,`removed`) NOT NULL | 기존 |
| `version` | bigint NOT NULL | 기존 |
| `joined_at` | timestamptz NOT NULL | 추가 |
| `ended_at` | timestamptz NULL | 추가 |
| `end_reason` | enum(`left_voluntarily`,`removed_by_owner`,`party_disbanded`,`account_deleted`) NULL | 추가 |
| `invite_id` | uuid NULL | 추가. 생성자 멤버십은 NULL |

#### `party_invites`

| 필드 | 타입 | 비고 |
|---|---|---|
| `id` | uuid PK | |
| `party_id` | uuid NOT NULL | 기존 |
| `token_hash` | bytea NULL | **변경**. SHA-256. 정리 작업이 값을 지울 수 있어 NULL을 허용한다 |
| `status` | enum(`active`,`revoked`,`expired`,`exhausted`) NOT NULL | 기존 |
| `expires_at` | timestamptz NOT NULL | 기존 |
| `max_uses` | smallint NOT NULL | 기존 |
| `used_count` | smallint NOT NULL DEFAULT 0 | 기존 |
| `created_by_membership_id` | uuid NOT NULL | 추가 |
| `revoked_at` | timestamptz NULL | 추가 |
| `last_used_at` | timestamptz NULL | 추가 |

#### 불변식

- 기존: 활성 멤버는 `(party_id, user_id) WHERE status = 'active'` partial unique index로 하나만 존재한다.
- 추가: **`status='active'`인 Party에 한해** `owner_membership_id`가 가리키는 멤버십은 같은 Party에 속하고 `status='active'`이며 `role='owner'`여야 한다. `disbanded` Party의 `owner_membership_id`는 마지막 방장을 가리키는 감사 참조이며 이 조건의 대상이 아니다.
- 추가: 한 Party에 `role='owner' AND status='active'`인 멤버십은 최대 1개다. `(party_id) WHERE role='owner' AND status='active'` partial unique index로 강제한다. `active` Party에서는 정확히 1개다.
  이 index는 **즉시 검증된다**(unique index는 지연할 수 없다). 따라서 T4는 기존 방장 강등과 대상 승격을 하나의 `UPDATE ... CASE` 문으로 처리할 수 없고, **강등을 먼저 실행한 뒤 승격**하는 별도 두 문으로 나눠야 한다. 아래 지연 검증 목록은 `owner_membership_id`의 3조건만 담당한다.
- 추가: `status='active'`인 Party는 활성 멤버가 1명 이상이다. 마지막 활성 멤버가 사라지는 트랜잭션은 반드시 같은 트랜잭션에서 Party를 `disbanded`로 만든다.
- 추가: `party_invites.used_count <= max_uses`를 CHECK constraint로 강제한다.
- 추가: `status='disbanded'`인 Party에는 활성 멤버십, 활성 초대, 활성 projection이 없다.
- 기존 유지: 상태 전이와 `sync_changes`/`outbox_jobs` 생성은 같은 트랜잭션에서 커밋한다.

#### 검증 시점

위 불변식 중 세 가지는 트랜잭션 **중간**에 일시적으로 깨진다. T1은 멤버십이 생기기 전에 `active` Party를 insert하고, T7은 멤버십을 종료하기 전에 Party를 `disbanded`로 바꾼다. 따라서 다음은 row 단위 CHECK가 아니라 `CONSTRAINT TRIGGER ... DEFERRABLE INITIALLY DEFERRED`로 **커밋 시점에** 검증한다.

- `active` Party의 `owner_membership_id` 3조건 (같은 Party, `status='active'`, `role='owner'`)
- `active` Party의 활성 멤버 1명 이상
- `disbanded` Party의 활성 하위 row 0건

`parties.owner_membership_id` FK도 같은 이유로 `DEFERRABLE INITIALLY DEFERRED`다. 반면 `NOT NULL`은 PostgreSQL에서 지연 대상이 아니므로 어떤 트랜잭션도 이 컬럼을 비워 둔 채 진행할 수 없다. T1이 멤버십 UUID를 미리 생성하는 이유가 이것이다.

`token_hash`는 `(token_hash) WHERE token_hash IS NOT NULL` 부분 unique index를 갖는다. 만료·소진된 초대의 `token_hash`는 90일 뒤 정리 작업에서 NULL로 지우고 row는 감사 목적으로 남긴다.
`account_backend_design.md` §8은 이 컬럼의 NULL 허용 여부를 명시하지 않았으므로, 이 정리 규칙을 성립시키기 위해 NULL 허용으로 확정한다. 전역 unique를 부분 unique로 바꾸는 것이 그 대가다.

### 5.2 Party 상태 전이

```text
(생성) ─▶ active ─▶ disbanded
```

`disbanded`는 종단 상태다. 복구는 없으며 같은 멤버로 다시 만들려면 새 Party를 생성한다.

### 5.3 멤버십 상태 전이

```text
(초대 수락 / Party 생성) ─▶ active ─┬─▶ left     (end_reason: left_voluntarily | account_deleted)
                                    └─▶ removed  (end_reason: removed_by_owner | party_disbanded)
```

- `left`와 `removed`는 종단 상태다. 재가입은 기존 row를 되살리지 않고 **새 멤버십 row**를 만든다.
- 이유: 참여 이력이 감사와 지표에 필요하고, partial unique index가 비활성 row의 중복을 이미 허용하므로 추가 제약이 필요 없다. 되살리기 방식은 `joined_at`, `invite_id`, `end_reason`의 의미를 망가뜨린다.
- 재가입은 새 초대 링크 수락으로만 가능하다. 강퇴된 사용자도 방장이 새 링크를 주면 다시 들어올 수 있다. 영구 차단 목록은 Launch MVP 범위 밖이다.
- 새 멤버십의 공개 수준은 이전 참여 값을 복원하지 않고 항상 `busyOnly`로 시작한다.

### 5.4 초대 상태 전이

```text
active ─┬─▶ revoked    (방장이 무효화, 또는 Party 해산)
        ├─▶ expired    (expires_at 경과. scheduler 또는 조회 시점 판정)
        └─▶ exhausted  (used_count == max_uses)
```

- 만료 판정은 scheduler가 주기적으로 반영하되, 수락 경로는 저장된 `status`를 신뢰하지 않고 `expires_at`을 다시 비교한다. 배치 지연이 유효성을 늘려주지 않는다.
- Party 해산과 소유권 위임 시 해당 Party의 모든 `active` 초대를 `revoked`로 바꾼다. 위임 시 무효화하는 이유는 이전 방장이 뿌린 링크가 새 방장 모르게 계속 멤버를 늘리는 것을 막기 위해서다.

### 5.5 트랜잭션 정의

모든 항목은 단일 PostgreSQL 트랜잭션이며 도메인 변경, projection 삭제, `sync_changes`, `outbox_jobs`를 함께 커밋한다.

**잠금 순서**: 모든 트랜잭션은 `parties` → `party_memberships` → `party_invites` 순으로 row를 잠근다. 이 순서를 지키지 않으면 초대 수락(초대→Party)과 해산·위임(Party→초대)이 서로를 기다리는 교착이 생긴다. 대상 Party row를 먼저 `FOR UPDATE`로 잠그는 것이 모든 Party mutation의 첫 단계다.

**`expected_version`의 대상**: 모든 Party mutation에서 `expected_version`은 `parties.version`을 의미한다. 멤버십 row의 version은 sync 투영용이며 요청 검증에 쓰지 않는다.

**해산 확인**: `/v1/parties/*` 경로의 mutation은 Party row를 잠근 직후, 권한 검사 이전에 다음 순서로 판정한다.

1. 요청자의 해당 Party 멤버십 **이력**을 상태와 무관하게 조회한다. 이력이 없으면 `404 not_found` (§6의 존재 은닉).
2. 이력이 있고 Party가 `disbanded`면 `410 party_disbanded`.
3. 그 외에는 권한 검사로 진행한다.

해산으로 전원이 비활성 멤버십이 되므로, 권한 검사를 먼저 하면 과거 멤버에게도 `403`/`404`만 나가 클라이언트가 로컬 정리 시점을 알 수 없다. 반대로 이력 조회 없이 `410`을 먼저 반환하면 아무 관계 없는 사용자가 UUID를 넣어 보는 것만으로 "존재했다가 해산된 Party"와 "없는 Party"를 구분할 수 있다. 두 요구를 모두 만족시키는 것이 위 순서다.

**`/v1/invites/*` 경로는 이 규칙에서 제외한다.** 미리보기와 수락은 비멤버가 호출하는 경로이므로 §4.2에 따라 만료·무효·미존재·해산을 모두 `404 not_found`로 통일한다. 이 경로의 `404`는 T3 step 2의 Party 상태 검사에서 나온다. T7 step 6은 `active` 초대만 `revoked`로 바꾸므로 해산된 Party에 `expired`·`exhausted` 초대가 남을 수 있고, 그 token도 `token_hash`로는 여전히 조회된다. 따라서 초대 상태만으로는 판정이 성립하지 않으며 **Party 상태 검사가 반드시 초대 상태 검사보다 앞서야 한다**. 어느 경우든 응답은 `404`이므로 token 보유자에게 Party의 과거 존재를 알려주지 않는다.

각 트랜잭션의 step 1은 이 확인을 마친 뒤를 가리킨다.

#### T1. Party 생성

1. 소유 Party 한도 검사
2. 멤버십 UUID를 애플리케이션에서 미리 생성한다
3. `parties` insert. `owner_membership_id`에 2단계의 UUID를 넣는다. FK가 `DEFERRABLE INITIALLY DEFERRED`이므로 아직 없는 행을 참조해도 커밋 시점에만 검증된다
4. `party_memberships` insert (`id`는 2단계의 UUID, `role='owner'`, `status='active'`)
5. `party_visibility_settings` insert (`visibility_level='busyOnly'`)
6. 생성자의 활성 generation `calendar_busy_facts`로부터 이 Party의 `busyOnly` `party_schedule_projections`를 생성한다 (§5.5.1)
7. `sync_changes`: 생성자에게 `party`, `party_membership`, `party_visibility_setting`, `party_schedule_projection` upsert

#### T2. 초대 생성

1. `parties` row를 잠그고 방장 권한, 요청 제한, 활성 초대 개수를 검사한다. 개수는 저장된 `status`가 아니라 `status='active' AND expires_at > now()`로 센다 (scheduler 지연이 부당한 거부를 만들지 않도록)
2. token 생성, hash만 저장
3. `sync_changes`: 방장에게만 `party_invite` upsert (token 원문 제외)
4. 응답에만 token 원문 포함

#### T3. 초대 수락

1. `token_hash`로 초대를 조회해 `party_id`를 얻는다 (잠금 없음)
2. 해당 `parties` row를 `FOR UPDATE`로 잠그고 `status='active'` 확인. 아니면 `404 not_found`. 이 검사는 **step 4의 초대 상태 검사보다 반드시 앞선다** — T7 step 6이 `active` 초대만 revoke하므로 해산된 Party에 `expired`·`exhausted` 초대가 남을 수 있고, Party 상태를 먼저 보지 않으면 §4.2의 404 통일이 깨진다
3. 초대 row를 `FOR UPDATE`로 잠근다 (잠금 순서: Party → 초대)
4. `expires_at > now()`, `status='active'`, `used_count < max_uses` 재검증
5. 이미 활성 멤버면 `409 already_member`
6. 잠긴 Party 아래에서 활성 멤버 수를 세고 `member_limit` 검사
7. 참여 Party 한도 검사
8. `party_memberships` insert (`role='member'`, `status='active'`, `invite_id`)
9. `party_visibility_settings` insert (`busyOnly`)
10. 신규 멤버의 활성 generation `calendar_busy_facts`로부터 이 Party의 `busyOnly` `party_schedule_projections`를 생성한다 (§5.5.1)
11. `used_count += 1`, `last_used_at` 갱신, 소진되면 `exhausted`
12. Party `version += 1`
13. `sync_changes`
    - 신규 멤버에게: Party 단위 부트스트랩 (`party`, 모든 `party_membership`, 허용되는 `party_schedule_projection`, 본인 `party_visibility_setting`)
    - 기존 활성 멤버 전원에게: `party` upsert, 신규 `party_membership` upsert, 신규 멤버의 `party_schedule_projection` upsert
14. `outbox_jobs`: `notify_member_joined` (dedupe `membership_id`)

#### T4. 소유권 위임

1. 요청자가 현재 방장인지, `expected_version` 일치 검사
2. 대상이 같은 Party의 활성 멤버이고 본인이 아닌지 검사
3. 기존 방장 멤버십을 `role='member'`로 **먼저** update하고, 그다음 대상 멤버십을 `role='owner'`로 update한다. owner partial unique index가 즉시 검증되므로 두 문의 순서를 지켜야 한다
4. `parties.owner_membership_id` 갱신, `version += 1`
5. 해당 Party의 `active` 초대를 모두 `revoked`
6. `sync_changes`
   - 활성 멤버 전원에게: `party` upsert와 두 `party_membership` upsert
   - **이전 방장과 새 방장에게만**: 무효화된 `party_invite` tombstone. 일반 멤버는 초대 entity를 볼 권한이 없으므로(§6, §9.2) 존재와 개수를 sync로도 알려주지 않는다
7. `outbox_jobs`: `notify_owner_transferred`
8. 커밋 시점에 §5.1 소유권 불변식이 지연 검증된다

#### T5. 탈퇴 (일반 멤버, 또는 위임을 마친 이전 방장)

1. 요청자가 활성 멤버인지 검사
2. `role='owner'`이고 다른 활성 멤버가 있으면 `409 owner_transfer_required`로 중단
3. `role='owner'`이고 단독 멤버면 T7(해산)으로 처리하되, 해당 멤버십만은 `status='left'`, `end_reason='left_voluntarily'`로 기록한다 (자발적 탈퇴를 강제 종료로 왜곡하지 않기 위해). **이 경우 T5의 이후 단계를 수행하지 않는다.** T7이 이미 멤버십을 종단 상태로 만들었으므로 step 4가 이어 실행되면 두 번 전이하게 된다
4. 멤버십 `status='left'`, `ended_at`, `end_reason='left_voluntarily'`, `version += 1`
5. 해당 `(party_id, user_id)`의 `party_schedule_projections` **삭제**
6. `party_visibility_settings` 삭제
7. Party `version += 1`
8. `sync_changes`
   - 탈퇴자에게: `party`, 모든 `party_membership`, 다른 멤버의 `party_schedule_projection`, `proposal` tombstone
   - 남은 멤버 전원에게: `party` upsert, 탈퇴자의 `party_membership` 상태 변경, 탈퇴자의 `party_schedule_projection` tombstone
9. `outbox_jobs`: `notify_member_left`
10. 진행 중 제안에 `membership_ended` 신호 발행 (§5.6)

#### T6. 강퇴

T5의 4~10단계를 재사용하되 **대상 멤버십을 요청자가 아닌 `{user_id}`의 활성 멤버십으로 치환한다**. 구체적으로는 다음과 같다.

1. 요청자가 방장인지, `expected_version`이 일치하는지 검사
2. `{user_id}`가 같은 Party의 활성 멤버이고 요청자 본인이 아닌지 검사. 본인이면 `400 invalid_request` (방장은 자기 자신을 강퇴할 수 없다)
3. T5의 step 2~3(방장 위임 강제, 단독 방장 해산 분기)은 **수행하지 않는다**. 강퇴 대상은 정의상 방장이 아니다
4. 이후 T5의 step 4~10을 대상 멤버십에 적용하되 `end_reason='removed_by_owner'`, 알림은 `notify_member_removed`를 쓴다

#### T7. 해산

1. 방장 권한과 `expected_version` 검사 (단독 방장의 탈퇴 경로에서는 권한 검사만)
2. `parties.status='disbanded'`, `disbanded_at`, `version += 1`
3. 모든 활성 멤버십 `status='removed'`, `end_reason='party_disbanded'` (T5 step 3에서 진입한 경우 그 멤버십만 `left`/`left_voluntarily`)
4. 해당 Party의 모든 `party_schedule_projections` 삭제
5. 모든 `party_visibility_settings` 삭제
6. 모든 `active` 초대 `revoked`
7. `sync_changes`: 마지막 활성 멤버 전원에게 `party` 상태 변경과 하위 entity tombstone (`party_membership`, `party_schedule_projection`, `party_visibility_setting`, `proposal`; 초대 tombstone은 방장에게만)
8. `outbox_jobs`: `notify_party_disbanded`. 단, **단독 방장의 탈퇴 경로에서는 발행하지 않는다** (수신자가 방금 실행한 본인 1명뿐이라 무의미하다)
9. 진행 중 제안에 `party_disbanded` 신호 발행 (§5.6)
10. 해당 Party의 미완료 `calendar_write_commands` 처리는 이 설계가 정하지 않는다. `calendar_privacy_sync_design.md` §10.1의 `pending_device` 명령이 해산 후에도 남아 지정 기기가 계속 claim하는 문제가 있으므로, `cancelled` 전이 여부와 시점을 설계 8에서 반드시 확정한다

#### 5.5.1 가입 시 projection 생성

`calendar_privacy_sync_design.md` §8은 공개 수준을 **변경**할 때의 projection 갱신만 정의하고, §7.2는 snapshot 완료 시 갱신을 정의한다. 새 멤버가 Party에 들어오는 순간은 둘 다 아니므로 이 설계가 채운다.

- **T1 step 6**은 생성자의, **T3 step 10**은 신규 멤버의 활성 generation `calendar_busy_facts`를 읽어 그 Party의 `busyOnly` projection을 만든다.
- T1을 빠뜨리면 첫 초대 수락 후 신규 멤버 화면에서 방장의 일정만 비어 보인다. Party 생성 시점에는 그 Party가 아직 없었으므로 생성자의 직전 snapshot 완료 경로도 이미 지나간 뒤다.
- 생성자 또는 신규 멤버에게 아직 `ready` 상태의 calendar connection이 없으면 projection을 만들지 않는다. 이후 첫 snapshot 완료 시 `calendar_privacy_sync_design.md` §7.2 경로가 생성한다.
- 이 단계가 없으면 가입자의 일정이 다음 snapshot 완료 전까지 다른 멤버 화면에 나타나지 않는다. 가능 시간 계산은 private busy facts를 직접 쓰므로 정상 동작해 결함 발견이 늦어진다.
- projection 생성은 각각 T1·T3와 같은 트랜잭션에서 커밋한다.

### 5.6 진행 중 제안과의 경계

`account_backend_design.md` §8 불변식에 따라 `proposal_participants`는 제안 생성 시점의 고정 snapshot이므로 탈퇴·강퇴·해산이 참여자 row를 삭제하지 않는다.

이 설계가 정의하는 것은 발행되는 신호까지다.

| 사건 | 발행 신호 | payload |
|---|---|---|
| 탈퇴·강퇴 | `membership_ended` | `party_id`, `user_id`, `end_reason`, `membership_version` |
| 해산 | `party_disbanded` | `party_id`, `party_version` |

다음은 이 설계의 범위가 아니며 설계 7(제안·응답·확정)과 설계 8(캘린더 쓰기)에서 확정한다.

- 떠난 참여자의 응답을 확정 조건에서 제외할지, 제안을 무효화할지
- 전원 수락과 최소 인원 중 어떤 확정 조건을 쓸지
- 이미 확정된 이벤트의 캘린더 쓰기·취소 명령을 떠난 사용자에 대해 어떻게 처리할지

다만 이 설계가 강제하는 최소 조건은 다음 두 가지다.

- 해산된 Party의 미확정 제안은 어떤 경로로도 새로 확정될 수 없다.
- 비활성 멤버십을 가진 사용자는 해당 Party의 제안·응답 API에 **새 응답을 제출할 수 없다**. 조회는 `404 not_found`, 응답 제출은 `403 forbidden`이다.

#### 설계 7에 남기는 제약

위 두 번째 조건은 설계 7의 선택지를 좁힌다. `proposal_participants`가 고정 snapshot이므로 떠난 사람도 참여자 row로 남는데 그 사람은 더 이상 응답할 수 없다. 따라서 설계 7이 **순수한 "전원 수락"을 확정 조건으로 고르면 멤버가 한 명이라도 떠난 제안은 영구히 확정 불가**가 된다.

이 설계는 그 분기를 대신 결정하지 않는다. 다만 설계 7은 다음 중 하나를 반드시 명시해야 한다.

- 떠난 참여자를 확정 조건 계산에서 제외한다
- 멤버가 떠나면 제안을 무효화하고 재제안을 요구한다
- 최소 인원 방식을 쓰고 떠난 참여자를 분모에서 뺀다

T5 step 8이 탈퇴자에게 `proposal` tombstone을 보내는 것은 탈퇴자 기기의 로컬 정리를 위한 것이며, 서버의 제안 상태를 바꾸지 않는다.

### 5.7 가능 시간 계산과의 상호작용

`calendar_privacy_sync_design.md` §9의 freshness gate는 **활성 멤버 전원**을 대상으로 한다. 따라서 멤버십 변화는 가능 시간 결과를 즉시 무효화한다.

- 가능 시간 응답은 계산에 사용한 `party_version`과 활성 멤버십 version 집합의 해시를 함께 반환한다.
- 클라이언트가 캐시한 결과의 해시가 현재 값과 다르면 결과를 재사용하지 않고 다시 조회한다.
- 방금 가입해 캘린더를 연결하지 않은 멤버가 있으면 Party 전체 가능 시간은 `calendar_sync_pending`이다. 이는 정상 동작이며 오류로 표시하지 않는다.
- 누구 때문에 pending인지는 다른 멤버에게 알리지 않는다. 본인이 원인일 때만 본인 화면에 캘린더 연결 CTA를 보여준다.
- 강퇴·탈퇴로 pending 원인이 사라지면 다음 조회부터 정상 결과가 나온다. 서버는 이를 별도로 알리지 않는다.

### 5.8 계정 삭제 연동

`account_backend_design.md` §5.3, §10의 계정 삭제 작업에 다음 단계를 추가한다. 협업 기록은 cascade 삭제가 아니라 restrict 후 익명화 대상이다.

1. 삭제 대상 사용자의 모든 활성 멤버십을 조회한다.
2. 각 Party에 대해 순서대로 처리한다.
   - 대상이 방장이고 다른 활성 멤버가 있으면 **강제 위임**한다. 승계자는 삭제 대상 본인을 제외하고 `users.status='active'`인 활성 멤버 중 `joined_at` 오름차순, 동률이면 `user_id` 오름차순으로 정한 첫 번째다. T4와 같은 트랜잭션 규칙을 따르되 알림 type은 `notify_owner_transferred_by_deletion`을 사용한다. 이후 아래 마지막 항목으로 진행한다.
   - 대상이 방장이고 **위 조건을 만족하는 승계자가 없으면** T7(해산)을 실행한다. 단독 멤버인 경우와, 남은 활성 멤버가 모두 삭제 진행 중인 경우가 여기에 해당한다. T7이 이미 해당 멤버십을 `removed`/`party_disbanded`로 종료하므로 **아래 종료 단계를 다시 실행하지 않는다**. `left`와 `removed`는 종단 상태이므로 두 번 전이할 수 없다(§5.3).
   - 위 두 경우가 아니거나 강제 위임을 마친 경우에만, T5와 동일하게 멤버십을 `left`, `end_reason='account_deleted'`로 종료하고 projection과 공개 설정을 삭제한다.
3. 종료된 멤버십 row는 유지하되 표시 이름을 참조하지 않는다. 남은 멤버의 이력 화면에는 고정 문구 `탈퇴한 사용자`를 표시한다.
4. 삭제 대상의 `party_schedule_projections`, `calendar_busy_facts`, `party_visibility_settings`는 남기지 않는다.

승계자를 사람이 고르지 않고 결정적 규칙으로 정하는 이유는, 삭제가 24시간 안에 끝나야 하는 비동기 작업이라 사용자 상호작용을 기다릴 수 없기 때문이다.
승계자 후보에서 `users.status`가 `active`가 아닌 사용자를 빼는 이유는, 삭제 진행 중인 사용자에게 넘기면 그 사용자의 삭제 작업이 곧바로 재승계를 일으켜 연쇄가 생기기 때문이다.

---

## 6. 역할별 권한

Launch MVP는 `owner`와 `member` 두 역할만 둔다. 중간 관리자 역할은 Post-MVP다.

| 작업 | owner | member | 비활성 멤버십 | 비멤버 |
|---|:---:|:---:|:---:|:---:|
| Party 조회 | O | O | X | X |
| 멤버 목록 조회 | O | O | X | X |
| Party 이름 수정 | O | X | X | X |
| 초대 링크 생성 | O | X | X | X |
| 활성 초대 목록 조회 | O | X | X | X |
| 초대 무효화 | O | X | X | X |
| 초대 미리보기 | 해당 없음 | 해당 없음 | 가능 | 가능(로그인 필요) |
| 초대 수락 | 해당 없음 | 이미 멤버 | 가능(새 멤버십) | 가능 |
| 소유권 위임 | O | X | X | X |
| 본인 공개 수준 변경 | O | O | X | X |
| 다른 멤버 공개 수준 변경 | X | X | X | X |
| 강퇴 | O (본인 제외) | X | X | X |
| 탈퇴 | 위임 후에만, 단독이면 해산 | O | X | X |
| 해산 | O | X | X | X |

- 방장은 다른 멤버의 공개 수준을 보거나 바꿀 수 없다. 관리 권한이 개인정보 권한으로 확장되지 않는다.
- 모든 권한 검사는 서버에서 수행한다. 클라이언트의 역할 표시는 화면 편의일 뿐 권한 근거가 아니다.
- 권한이 없는 대상에는 `403 forbidden`을, 멤버가 아닌 사용자가 Party를 지목하면 존재 여부를 숨기기 위해 `404 not_found`를 반환한다.

---

## 7. 개인정보 노출 규칙

### 7.1 멤버 간 노출

| 데이터 | 다른 활성 멤버에게 |
|---|---|
| 표시 이름 | 공개 |
| 역할 | 공개 |
| 가입 시각 | 공개 |
| Apple subject, 이메일, 전화번호 | 비공개 |
| 다른 Party 참여 목록 | 비공개 |
| 캘린더 연결 상태와 sync 상태 | 비공개 (본인만) |
| 공개 수준 설정값 | 비공개 (본인만) |
| 일정 | `calendar_privacy_sync_design.md` §3.3 규칙만 적용 |

공개 수준 설정값 자체를 감추는 이유는, `hidden`을 선택했다는 사실이 노출되면 특정 시간대에 감춘 일정이 있다는 추론의 출발점이 되기 때문이다.

### 7.2 초대 링크 보유자에게

- Party 이름, 활성 멤버 수, 초대자 표시 이름만 노출한다.
- 멤버 명단, 일정, 제안, 다른 초대 링크는 수락 전에 노출하지 않는다.
- 토큰 자체가 접근 자격이므로 유출 피해를 만료(기본 7일), `max_uses`(최대 10), 무효화 기능, 정원 상한으로 제한한다.

### 7.3 탈퇴·강퇴·해산 이후

- 떠난 사용자의 `party_schedule_projections`는 트랜잭션 안에서 삭제하고 tombstone을 보낸다. 유예 기간을 두지 않는다.
- 떠난 사용자는 tombstone을 받아 로컬 SwiftData에서 해당 Party의 모든 데이터를 지운다.
- `calendar_busy_facts`는 개인 소유 데이터이므로 유지한다. 다른 Party의 계산에 계속 쓰인다.
- 떠난 사용자의 표시 이름은 남은 멤버의 제안 이력에서 계속 보일 수 있다. 이는 협업 기록 보존이며, 계정 삭제 시에만 `탈퇴한 사용자`로 익명화한다.
- 강퇴 사유는 수집하지도 전달하지도 않는다.

### 7.4 금지 사항

- 초대 token 원문을 로그, audit event, 분석 이벤트, 푸시 payload에 기록하지 않는다.
- 푸시 payload에는 `party_changed`, `membership_changed` 같은 opaque type과 식별자만 담고 Party 이름과 멤버 이름을 담지 않는다.
- `audit_events`에는 actor, action, target ID, result, request ID만 남기고 Party 이름과 token을 남기지 않는다.

---

## 8. 화면 상태: 정상·빈 상태·로딩·오류·오프라인

### 8.1 상태 표

| 화면 | 정상 | 빈 상태 | 로딩 | 오류 | 오프라인 |
|---|---|---|---|---|---|
| Party 목록 | 카드 목록 | "아직 Party가 없습니다" + 생성 CTA | skeleton 3행 | 마지막 로컬 데이터 + 재시도 배너 | 로컬 데이터 + 오프라인 배지 |
| Party 상세 | 멤버·가능 시간·제안 | 멤버 1명이면 초대 CTA 강조 | 섹션별 skeleton | 섹션별 재시도 | 로컬 투영 표시, mutation은 큐 적재 |
| 초대 미리보기 | 이름·인원·수락 | 해당 없음 | 전체 화면 spinner | 만료/무효/정원초과 안내 | "연결 후 다시 시도" (큐 적재 금지) |
| 멤버 목록 | 목록 | 해당 없음 | skeleton | 재시도 | 로컬 목록 + 관리 동작 비활성 |

### 8.2 오프라인 큐 정책

`account_backend_design.md` §7.3의 로컬 mutation 큐를 사용하되 Party 작업은 다음으로 구분한다.

| 작업 | 오프라인 큐 | 이유 |
|---|---|---|
| Party 생성 | 허용 | 신규 생성이라 다른 사용자와 충돌하지 않는다. 다만 소유 Party 한도(§4.3)로 서버가 거부할 수 있으므로 실패 시 로컬 롤백과 사유를 안내한다 |
| Party 이름 수정 | 허용 | 버전 충돌 시 서버 값 우선 후 사용자에게 알림 |
| 탈퇴 (일반 멤버) | 허용 | 지연돼도 의도가 뒤집히지 않는다 |
| 탈퇴 (방장) | 금지 | 위임이 선행돼야 하므로 서버가 `409 owner_transfer_required`로 거부한다. 낙관적 성공이 거짓말이 된다 |
| 초대 생성 | 금지 | token은 서버가 만들어야 하며 오프라인에서 링크를 줄 수 없음 |
| 초대 수락 | 금지 | 정원·만료 검사가 서버 전용이라 낙관적 성공이 거짓말이 됨 |
| 소유권 위임 | 금지 | 대상이 그 사이 탈퇴하면 불변식이 깨짐 |
| 강퇴 | 금지 | 되돌릴 수 없고 대상 상태가 바뀌었을 수 있음 |
| 해산 | 금지 | 파괴적이며 되돌릴 수 없음 |

금지 작업은 오프라인에서 버튼을 비활성화하고 "연결이 필요합니다"를 표시한다.

### 8.3 오류 코드 매핑

기존 코드는 `account_backend_design.md` §6을 그대로 쓰고, Party 도메인 코드를 추가한다.

| HTTP | code | 의미 | 화면 |
|---:|---|---|---|
| 409 | `party_full` | 활성 멤버가 `member_limit`에 도달 | "정원이 찼습니다" |
| 409 | `already_member` | 이미 활성 멤버 | 수락 없이 Party 상세로 이동 |
| 409 | `owner_transfer_required` | 방장이 위임 없이 탈퇴 시도 | 위임 화면으로 유도 |
| 409 | `party_invite_limit_reached` | 활성 초대 3개 초과 | 기존 링크 재사용/무효화 안내 |
| 409 | `party_owned_limit_reached` | 소유 Party 10개 초과 | 정리 안내 |
| 409 | `party_joined_limit_reached` | 참여 Party 20개 초과 | 정리 안내 |
| 410 | `party_disbanded` | 해산된 Party 대상 작업 | 목록으로 이동 후 로컬 정리 |
| 404 | `not_found` | 무효·만료·미존재 초대, 비멤버의 Party 지목 | "초대가 유효하지 않습니다" |
| 429 | `rate_limited` | 초대 생성/수락 시도 제한 초과 | 재시도 시각 안내 |

만료 초대에 별도 코드를 주지 않고 `404 not_found`로 통일하는 이유는 §4.2와 같다.

`410`은 `account_backend_design.md` §6에서 이미 `sync_cursor_expired`가 쓰고 있다. 클라이언트는 상태 코드가 아니라 `code` 값으로 분기하며, **`sync_cursor_expired`만 전체 재동기화(bootstrap)를 트리거한다**. `party_disbanded`는 해당 Party의 로컬 정리와 목록 이동만 일으킨다.

해산된 Party에 대한 응답은 조회와 mutation을 구분한다. 해산으로 전원이 비활성 멤버십이 되므로 §6의 존재 은닉 규칙에 따라 **조회는 `404 not_found`**, 이미 목록에 남은 항목을 대상으로 한 **mutation은 `410 party_disbanded`**를 반환해 클라이언트가 로컬 정리를 수행하게 한다.

---

## 9. API 계약과 외부 연동

### 9.1 Endpoint

`account_backend_design.md` §6의 공통 규칙(`/v1`, `Idempotency-Key`, `If-Match`/`expected_version`, `request_id`, 버전 반환)을 모두 적용한다.

| Method | Path | 권한 | 비고 |
|---|---|---|---|
| POST | `/v1/parties` | 인증 사용자 | 생성. T1 |
| GET | `/v1/parties/{id}` | 활성 멤버 | 상세 |
| PATCH | `/v1/parties/{id}` | owner | 이름 수정. `expected_version` 필수 |
| POST | `/v1/parties/{id}/disband` | owner | 해산. T7. `expected_version` 필수 |
| GET | `/v1/parties/{id}/memberships` | 활성 멤버 | 멤버 목록 |
| DELETE | `/v1/parties/{id}/memberships/me` | 활성 멤버 | 탈퇴. T5 |
| DELETE | `/v1/parties/{id}/memberships/{user_id}` | owner | 강퇴. T6. `expected_version` 필수 |
| POST | `/v1/parties/{id}/owner` | owner | 위임. T4. `expected_version` 필수 |
| POST | `/v1/parties/{id}/invites` | owner | 초대 생성. T2. token 원문 1회 반환 |
| GET | `/v1/parties/{id}/invites` | owner | 활성 초대 목록. token 원문 미포함 |
| DELETE | `/v1/parties/{id}/invites/{invite_id}` | owner | 무효화 |
| GET | `/v1/invites/{token}/preview` | 인증 사용자 | 미리보기. §7.2 필드만 |
| POST | `/v1/invites/{token}/accept` | 인증 사용자 | 가입. T3 |

`{token}`은 URL path에 담기므로 접근 로그에 원문이 남지 않도록 웹 서버와 프록시의 path 로깅을 비활성화하거나 마스킹한다. 이 요구를 만족시키지 못하는 배포 환경에서는 token을 요청 본문으로 옮긴다.

Universal Link를 위해 다음이 함께 필요하다.

- 서비스 도메인 루트에 `/.well-known/apple-app-site-association`를 `application/json`으로, 리다이렉트 없이 HTTPS로 제공한다. `applinks` 항목의 경로 패턴은 `/i/*`다.
- 앱 미설치 사용자가 `https://<도메인>/i/{token}`을 열면 같은 URL이 웹 폴백 페이지를 반환한다. 이 페이지는 App Store 링크와 Party 이름만 보여주고, 초대 유효성 판정이나 멤버 정보를 노출하지 않는다.
- 폴백 페이지도 §4.2의 미리보기 조회 제한을 동일하게 적용한다.

### 9.2 동기화

- 모든 Party 변경은 `sync_changes`에 `recipient_user_id`별 row로 기록한다.
- entity type: `party`, `party_membership`, `party_invite`, `party_visibility_setting`, `party_schedule_projection`, `proposal`.
  `proposal`은 T5·T7이 tombstone으로만 발행하며 생성·갱신 계약은 설계 7이 소유한다.
  `party_schedule_projection`은 T1·T3가 upsert를, T5·T7이 tombstone을 발행한다. 그 외의 생성·갱신 계약은 `calendar_privacy_sync_design.md`가 소유한다.
- 탈퇴·강퇴·해산은 tombstone(`operation='delete'`)으로 전달하며 떠난 사용자와 남은 사용자 **양쪽 모두**에게 보낸다.
- 초대 수락 직후 신규 멤버는 해당 Party 하위 데이터를 아직 갖고 있지 않으므로, 서버는 신규 멤버 대상 변경 row를 Party 단위 부트스트랩으로 묶어 발행한다. cursor가 이미 앞서 있어도 누락되지 않는다.
- `party_invite` 변경은 현재 방장에게만 전달한다. 위임(T4)의 무효화 tombstone만 예외적으로 직전 방장에게도 전달해 그 기기의 로컬 목록이 정리되게 한다.

### 9.3 알림 (outbox job)

| job type | dedupe_key | 수신자 |
|---|---|---|
| `notify_member_joined` | `membership_id` | 신규 멤버를 제외한 활성 멤버 |
| `notify_member_left` | `membership_id` | 남은 활성 멤버 |
| `notify_member_removed` | `membership_id` | 강퇴 대상과 남은 활성 멤버 |
| `notify_owner_transferred` | `party_id:party_version` | 활성 멤버 전원 |
| `notify_owner_transferred_by_deletion` | `party_id:party_version` | 활성 멤버 전원 |
| `notify_party_disbanded` | `party_id` | 해산 직전 활성 멤버 |

- dedupe key는 `account_backend_design.md` §9의 partial unique index로 중복 발행을 막는다.
- 문구, urgency, quiet hours, 묶음 처리는 설계 9에서 확정한다.

### 9.4 재시도와 중복 방지

- 초대 수락 재시도: 같은 `Idempotency-Key`면 저장된 결과를 재사용한다. 키가 다르고 이미 멤버면 `409 already_member`를 반환하며 `used_count`를 두 번 올리지 않는다.
- 정원 경쟁: 두 사용자가 마지막 자리를 동시에 수락하면 `parties` row 잠금 아래 카운트를 세므로 한 명만 성공하고 나머지는 `409 party_full`을 받는다.
- 소유권 위임 경쟁: `expected_version` 불일치로 두 번째 요청이 `409 version_conflict`를 받는다.
- 탈퇴 재시도: 이미 비활성이면 현재 상태와 `200`을 반환한다.
- 멱등성 결과 재사용 시에도 현재 권한을 먼저 평가한다. 이미 Party를 떠난 사용자는 과거 성공 payload 대신 `403`/`404`를, 해산된 Party에 과거 멤버십이 있는 사용자는 `410 party_disbanded`를 받는다.

---

## 10. 테스트 가능한 인수 조건

### 생성과 이름

- [ ] Party 생성 시 생성자가 `owner`이고 활성 멤버 1명이며 `owner_membership_id`가 그 멤버십을 가리킨다.
- [ ] 생성자의 공개 수준이 `busyOnly`로 초기화된다.
- [ ] 이름이 41자 이상, 빈 문자열, 공백만, 제어문자를 포함하면 `400 invalid_request`다.
- [ ] 일반 멤버의 이름 수정은 `403 forbidden`이다.
- [ ] 낡은 `expected_version`으로 이름을 수정하면 `409 version_conflict`이고 서버 값이 바뀌지 않는다.

### 스키마와 트랜잭션 무결성

- [ ] T1이 커밋되고, 커밋 시점에 `owner_membership_id`가 실제 활성 owner 멤버십을 가리킨다.
- [ ] T7이 커밋되고, disbanded Party에 활성 멤버십·초대·projection이 0건이다.
- [ ] 만료된 초대의 `token_hash`를 NULL로 지우는 정리 작업이 제약 위반 없이 성공한다.
- [ ] 초대 수락과 해산을 동시에 실행해도 교착이 발생하지 않는다.
- [ ] `member_limit`을 11 이상으로 바꾸는 update가 CHECK constraint로 거부된다.

### 초대

- [ ] 초대 응답 본문에만 token 원문이 있고 DB에는 hash만 저장된다.
- [ ] 만료된 token, 무효화된 token, 존재하지 않는 token이 모두 동일한 `404 not_found`를 반환한다.
- [ ] `max_uses`에 도달한 초대는 `exhausted`가 되고 이후 수락이 `404 not_found`다.
- [ ] `expires_at`이 지난 초대는 scheduler가 아직 돌지 않았어도 수락이 거부된다.
- [ ] 활성 초대 4개째 생성이 `409 party_invite_limit_reached`다.
- [ ] 미로그인 상태로 수락 API를 호출하면 `401 invalid_session`이고 어떤 멤버십도 생성되지 않는다.
- [ ] 초대 token이 애플리케이션 로그, audit event, 분석 이벤트, 푸시 payload 어디에도 나타나지 않는다.
- [ ] 미리보기 조회가 분당 10회를 넘으면 `429 rate_limited`다.
- [ ] 정원이 찬 Party에서 초대 생성이 `409 party_full`이고 `max_uses=0`인 초대가 만들어지지 않는다.
- [ ] 이미 활성 멤버인 사용자의 수락이 `409 already_member`이고 `used_count`가 증가하지 않는다.

### 정원과 한도

- [ ] 활성 멤버 10명인 Party에 수락하면 `409 party_full`이다.
- [ ] 10명 정원의 마지막 자리를 두 요청이 동시에 수락하면 정확히 하나만 성공한다.
- [ ] 강퇴로 자리가 비면 다음 수락이 성공한다.
- [ ] 소유 Party 11개째 생성이 `409 party_owned_limit_reached`다.

### 역할과 소유권

- [ ] `active` Party에 `role='owner' AND status='active'` 멤버십이 항상 정확히 1개이고, `disbanded` Party에는 0개다.
- [ ] 위임 후 이전 방장은 `member`가 되고 관리 API가 모두 `403 forbidden`이다.
- [ ] 위임 시 해당 Party의 활성 초대가 모두 `revoked`가 된다.
- [ ] 위임 시 일반 멤버의 sync 응답에 `party_invite` tombstone이 포함되지 않는다.
- [ ] 비활성 멤버를 대상으로 위임하면 `400 invalid_request`이고 소유권이 바뀌지 않는다.
- [ ] 위임이 강등→승격 두 문으로 실행되어 owner partial unique index 위반 없이 커밋된다.
- [ ] 방장이 자기 자신을 강퇴하면 `400 invalid_request`다.

### 탈퇴·강퇴·해산

- [ ] 다른 활성 멤버가 있는 방장의 탈퇴가 `409 owner_transfer_required`다.
- [ ] 단독 방장이 탈퇴하면 Party가 `disbanded`가 되고 활성 멤버십·초대·projection이 남지 않는다.
- [ ] 탈퇴 트랜잭션 커밋 후 해당 사용자의 `party_schedule_projections`가 그 Party에 0건이다.
- [ ] 탈퇴 후에도 그 사용자의 `calendar_busy_facts`는 남아 다른 Party 계산에 쓰인다.
- [ ] 탈퇴자와 남은 멤버 양쪽의 `sync_changes`에 각각 tombstone이 기록된다.
- [ ] 강퇴된 사용자가 그 Party의 조회 API에서 `404 not_found`를, 응답 제출 API에서 `403 forbidden`을 받는다.
- [ ] 해산된 Party에 **과거 멤버십이 있는 사용자**의 mutation이 `403`/`404`가 아니라 `410 party_disbanded`다.
- [ ] 해산된 Party에 **한 번도 멤버였던 적이 없는 사용자**의 mutation은 `404 not_found`다 (`410`으로 존재를 드러내지 않는다).
- [ ] 해산된 Party의 초대 token 수락이 `410`이 아니라 `404 not_found`다. 해산 시점에 `expired`·`exhausted`였던(따라서 revoke되지 않은) 초대도 마찬가지로 `404`다.
- [ ] 강퇴된 사용자가 새 초대 링크로 재가입하면 새 멤버십 row가 생기고 공개 수준이 `busyOnly`로 시작한다.

### 계정 삭제

- [ ] 방장이 계정을 삭제하면 `joined_at`이 가장 이른 활성 멤버가 방장이 되고 Party는 `active`를 유지한다.
- [ ] 단독 방장이 계정을 삭제하면 Party가 `disbanded`가 된다.
- [ ] 삭제된 사용자의 멤버십 row는 남지만 표시 이름 대신 `탈퇴한 사용자`가 렌더링된다.
- [ ] 삭제된 사용자의 projection과 공개 설정이 모든 Party에서 0건이다.
- [ ] 승계자 후보가 모두 삭제 진행 중이면 Party가 `disbanded`가 되고 멤버십 종료가 한 번만 일어난다.
- [ ] 단독 방장이 자발적으로 탈퇴하면 그 멤버십의 `end_reason`이 `left_voluntarily`이고 `notify_party_disbanded`가 발행되지 않는다.

### 가능 시간 연동

- [ ] 캘린더가 `ready`인 사용자가 Party를 생성하면 같은 트랜잭션에서 생성자의 `busyOnly` projection이 만들어지고, 첫 초대 수락자 화면에 방장의 일정이 보인다.
- [ ] 캘린더가 `ready`인 사용자가 가입하면 같은 트랜잭션에서 `busyOnly` projection이 생성되어 다른 멤버 화면에 즉시 나타난다.
- [ ] 새 멤버가 캘린더를 연결하지 않으면 Party 가능 시간이 `calendar_sync_pending`이고 projection은 생성되지 않는다.
- [ ] `calendar_sync_pending` 응답이 원인 멤버의 ID나 수를 포함하지 않는다.
- [ ] 멤버가 추가·제거되면 이전 가능 시간 응답의 멤버십 해시가 달라져 클라이언트가 캐시를 재사용하지 않는다.

### 오프라인

- [ ] 오프라인에서 Party 생성과 탈퇴가 큐에 쌓이고 복귀 후 정확히 한 번 반영된다.
- [ ] 오프라인에서 초대 생성·수락·위임·강퇴·해산 버튼이 비활성화된다.
- [ ] 방장인 사용자의 탈퇴는 오프라인에서 큐에 쌓이지 않는다.

---

## 11. 분석 지표와 민감정보 제외 기준

### 수집 지표

| 이벤트 | 속성 |
|---|---|
| `party_created` | 익명 user key, 소유 Party 수 구간 |
| `party_invite_created` | 익명 user key, `max_uses`, 만료 기간(일) |
| `party_invite_accepted` | 익명 user key, 초대 생성~수락 경과 시간 구간, 수락 시 활성 멤버 수 |
| `party_invite_rejected_reason` | `expired` / `exhausted` / `party_full` / `already_member` 중 하나 |
| `party_member_left` | `end_reason`, 참여 기간 구간 |
| `party_owner_transferred` | 트리거(`manual` / `account_deletion`) |
| `party_disbanded` | 해산 시 멤버 수 구간, Party 수명 구간 |
| `party_size_distribution` | 활성 Party의 멤버 수 히스토그램 (일 1회 집계) |

- 사용자 식별자는 `user_id`를 분석 전용 salt로 해시한 값만 쓴다.
- 멤버 수와 기간은 원값 대신 구간(1, 2-3, 4-6, 7-10)으로 보낸다.

### 금지 데이터

- Party 이름, 초대 token 원문 또는 그 앞부분
- 사용자 표시 이름, 이메일, Apple subject
- 멤버 명단, 일정 제목·장소·시간
- 강퇴 대상과 실행자의 식별 가능한 조합
- 푸시 payload에 담긴 어떤 도메인 문자열

### 운영 지표

- 초대 수락 성공률, 수락 실패 사유 분포
- 정원 초과 발생률
- 소유권 위임 없이 탈퇴를 시도한 비율 (UX 유도 실패 신호)
- 멤버십 변경 트랜잭션의 p95 지연과 `version_conflict` 비율

---

## 12. 미결정 사항과 후속 범위

| 항목 | 넘길 곳 | 이유 |
|---|---|---|
| 떠난 참여자가 있는 제안의 확정 조건 | 설계 7 | 전원 수락 vs 최소 인원 결정에 종속. 단 §5.6이 "비활성 멤버는 새 응답을 제출할 수 없다"를 강제하므로, 떠난 참여자가 남아 있는 제안에는 순수 전원 수락을 그대로 적용할 수 없다 |
| 확정된 이벤트의 캘린더 쓰기를 떠난 사용자에게 취소할지 | 설계 8 | write command 생명주기 소관 |
| 해산된 Party의 미완료 `calendar_write_commands` 처리 | 설계 8 | T7 step 10. 방치하면 지정 기기가 계속 claim한다 |
| 초대·탈퇴·강퇴 알림의 문구, urgency, quiet hours | 설계 9 | 알림 정책 일괄 확정 |
| 무료 Party 개수 제한과 Premium 상향 | Post-MVP 결제 | §4.3 시스템 한도와 별개 |
| 중간 관리자(`admin`) 역할 | Post-MVP | 2역할로 Launch MVP 시나리오가 충족됨 |
| 영구 차단 목록(강퇴자 재가입 금지) | Post-MVP | 남용 신고가 실제로 발생한 뒤 설계 |
| QR·이메일·전화번호 초대와 비회원 초대 | Post-MVP | pre-account 상태와 계정 병합 로직이 필요 |
| Party 이름 외 설명·아이콘·색상 | Post-MVP | 핵심 시나리오에 불필요 |
| 멤버 상한 10명의 상향 | 출시 후 재검토 | 실제 Party 크기 분포(`party_size_distribution`) 확인 후 결정 |

---

## 13. 현재 구현과의 차이

`src/PlanTogether/Models.swift`, `AppStore.swift` 기준.

| 현재 | 설계 | 필요한 변경 |
|---|---|---|
| `Party`에 `members: [Member]`만 있고 역할·상태·소유자가 없다 | `owner`/`member` 역할, 멤버십 상태, `ownerMembershipID` | `Membership` 타입 신설, `Party`가 멤버십 목록을 보유 |
| 멤버십 개념 자체가 없다 | 멤버십이 독립 entity이며 이력이 남는다 | `Member`는 사용자 프로필, `Membership`은 참여 상태로 분리 |
| 초대 개념이 없다 | 초대 링크 entity와 4개 상태 | `PartyInvite` 타입과 수락 흐름 신설 |
| Party 상태가 없다 | `active` / `disbanded` | `PartyStatus` 추가 |
| 멤버 상한이 없다 | 활성 멤버 10명 | 생성·수락 경로에 검사 추가 (서버가 최종 판정) |
| 데이터가 메모리 전용이고 서버가 없다 | 서버 기준 데이터 + SwiftData 투영 + 오프라인 큐 | 저장소 계층과 sync 클라이언트 신설 |
| `visibilityByMember`가 Party 안의 dictionary다 | `party_visibility_settings`가 본인만 읽고 쓰는 별도 entity | 다른 멤버의 값을 클라이언트가 보유하지 않도록 분리 |
| `AvailabilitySlot`이 `availableMemberCount`와 `totalMemberCount`를 노출한다 | 전원 공통 슬롯만 반환하고 인원수를 노출하지 않는다 | 두 필드 모두 제거 (`calendar_privacy_sync_design.md` §4와도 충돌) |
| 탈퇴·강퇴·해산 경로가 없다 | 세 경로 모두 projection 삭제와 tombstone을 동반 | 상태 전이와 로컬 정리 로직 신설 |

이 설계는 문서 확정 단계이며 Swift 코드는 변경하지 않는다. 기존 코드는 계속 빌드·실행 가능한 상태로 둔다.

---

## 14. 구현 순서

1. 스키마 마이그레이션: `parties`, `party_memberships`, `party_invites` 확장, partial unique index 3종, `owner_membership_id` FK의 `DEFERRABLE INITIALLY DEFERRED` 선언, §5.1 검증 시점의 지연 `CONSTRAINT TRIGGER` 3종, `party_visibility_settings`·`party_schedule_projections` 최소 스키마
2. Party 생성·조회·이름 수정 API와 권한 검사
3. 초대 생성·무효화·미리보기·수락 API와 정원·만료·경쟁 처리
4. 소유권 위임 API와 불변식 검증
5. 탈퇴·강퇴·해산 트랜잭션과 projection 삭제·tombstone 발행
6. `sync_changes` 전파와 신규 멤버 부트스트랩
7. outbox 알림 job 발행 (전송 정책은 설계 9)
8. 계정 삭제 작업의 강제 위임·해산 단계 추가
9. iOS 화면과 오프라인 큐 정책 적용
10. §10 인수 조건 자동화

1~5는 서버 단독으로 검증 가능하며 iOS 변경 없이 진행한다.

---

## 15. 검증 계획

### 서버 단위 테스트

- 이름 정규화·검증 경계값
- 초대 token 생성·hash 비교·만료 판정
- 상태 전이 함수의 허용/거부 조합 전수
- 승계자 선정 규칙의 결정성 (`joined_at` 동률 포함)

### PostgreSQL 통합 테스트

- partial unique index가 두 번째 활성 멤버십과 두 번째 활성 방장을 거부하는지
- 정원 경쟁을 동시 트랜잭션으로 재현해 정확히 하나만 성공하는지
- 탈퇴 트랜잭션이 projection 삭제와 `sync_changes` 기록을 원자적으로 커밋하는지
- 해산 후 Party 하위 활성 row가 0건인지
- 멱등성 키 재사용 시 `used_count`가 한 번만 증가하는지

### API 계약 테스트

- 권한 표(§6)의 모든 조합에 대한 응답 코드
- `expected_version` 누락·불일치 처리
- 오류 코드 매핑(§8.3)의 전수 확인

### E2E

- 방장이 링크를 만들어 3명이 가입 → 각자 공개 수준 설정 → 가능 시간 조회 → 멤버 1명 탈퇴 → 남은 인원으로 재조회
- 방장 위임 후 이전 방장 탈퇴 → Party 유지 확인
- 단독 방장 탈퇴 → 해산과 로컬 정리 확인
- 방장 계정 삭제 → 강제 위임 후 Party 유지 확인

### Privacy 회귀

- 어떤 API 응답에도 다른 멤버의 공개 수준, 캘린더 연결 상태, 이메일이 없다.
- 초대 미리보기 응답에 멤버 명단과 일정이 없다.
- 탈퇴·해산 이후 남은 멤버의 sync 응답에 떠난 사용자의 projection이 없다.
- 로그·분석·푸시 전 경로에 초대 token과 Party 이름이 없다.

---

## 16. 리스크와 완화

| 리스크 | 영향 | 완화 |
|---|---|---|
| 초대 링크 유출 | 모르는 사람의 가입 | 7일 만료, `max_uses` 최대 10, 즉시 무효화, 정원 10명, 방장이 강퇴 가능 |
| 방장 부재 | 관리 작업 불가 | 위임 강제, 계정 삭제 시 결정적 강제 승계, `owner_membership_id` NOT NULL |
| 멤버 변경으로 인한 계산 캐시 불일치 | 잘못된 가능 시간 표시 | 응답에 멤버십 해시 포함, 불일치 시 재조회 |
| 정원 경쟁으로 인한 초과 가입 | 불변식 위반 | `parties` row 잠금 아래 카운트, DB 레벨 재검증 |
| 2인 Party에서의 hidden 추론 | 개인정보 노출 | `calendar_privacy_sync_design.md` §4의 한계를 설정 화면에 그대로 설명 |
| 탈퇴 후 projection 잔존 | 개인정보 노출 | 같은 트랜잭션 삭제, Privacy 회귀 테스트로 상시 검증 |
| token이 URL path에 노출 | 접근 로그 유출 | path 로깅 마스킹, 불가하면 요청 본문으로 이동 |

---

## 17. 참고 공식 문서

- [Supporting universal links in your app](https://developer.apple.com/documentation/xcode/supporting-universal-links-in-your-app)
- [Allowing apps and websites to link to your content](https://developer.apple.com/documentation/xcode/allowing-apps-and-websites-to-link-to-your-content)
- [Sign in with Apple REST API](https://developer.apple.com/documentation/signinwithapplerestapi)
- [PostgreSQL - Unique Indexes (partial index)](https://www.postgresql.org/docs/current/indexes-partial.html)
- [PostgreSQL - Explicit Locking](https://www.postgresql.org/docs/current/explicit-locking.html)
