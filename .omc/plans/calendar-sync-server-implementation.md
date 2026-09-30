# 캘린더 동기화 서버 구현 계획 (시연 트랙)

기준 설계: `docs/calendar_privacy_sync_design.md` §3, §5.2, §6, §7, §8, §11, §12
선행: Party P1~P3 (`party_schedule_projections`, `party_visibility_settings` 스키마), 요청 제한

## 범위

시연 목표(두 사람의 캘린더에서 공통 가능 시간)에 필요한 **서버 최소 조각**만 설계대로 구현한다. 설계와 다르게 줄이지 않고, 아직 하지 않는 것을 명시한다.

### 이번에 하는 것

- 연결(`calendar_connections`), 읽기 source 선택, snapshot session·staging, 활성 generation의 `calendar_busy_facts`
- API: `GET/PUT /v1/calendar/connection`, snapshot 생성·page 업로드·complete·abort
- complete 트랜잭션: 검증 → generation 원자 교체 → 사용자의 활성 Party별 `busyOnly` projection 재생성 → `sync_changes` → 연결 `ready`
- Party 생성(T1)·가입(T3)의 busy projection stub 채우기(설계 4 §5.5.1)
- freshness 판정 헬퍼(가능 시간 검색 서버가 사용)
- staging 24시간 정리, 권한 철회(`revoked`) 시 projection 즉시 비노출

### 이번에 하지 않는 것 (근거)

- **details 수준의 제목·장소 ciphertext와 details projection**: 공개 수준 단계(V1)에서 한다. 이번 facts는 시간·종일·시간대·availability만 갖는다.
- **`calendar_writer_device_id`, write command, `app_confirmed_event_id`**: 캘린더 쓰기(설계 8)에서.
- 기기 전환(sync device 교체) 흐름의 전체: 활성 sync device 하나만 지원하고 교체는 새 기기가 `PUT connection`으로 요청하는 최소 형태만.
- 7일 뒤 권한 철회 facts 삭제 scheduler: 상태 전이와 즉시 비노출까지. 삭제 job은 후속.

## 단계

### C1. 스키마 (migration 00012)

- `calendar_connections`: user(unique), sync_device_id(devices FK, 없으면 NULL), permission_status·source_status(설계 §5.2 enum), active_generation, last_completed_sync_at, time_zone, version
- `calendar_source_selections`: connection, opaque source key(기기 로컬 키의 해시가 아니라 클라이언트가 만든 불투명 문자열), enabled. calendar 이름 저장 금지
- `calendar_snapshot_sessions`: connection, revision, window_start/end, expected_pages, status(open/completed/aborted/expired), UNIQUE(connection, revision)
- `calendar_snapshot_facts_staging`: session, page, source_event_key, start/end UTC, all_day, time_zone, availability
- `calendar_busy_facts`: connection, generation, user, source_event_key, start/end, all_day, time_zone, availability. (user_id, start_at, end_at) index, active key unique
- FK 삭제 정책: 사용자 소유 개인 데이터라 CASCADE(설계 §11, 계정 삭제 §12)
- 완료 기준: 통합 테스트로 제약(time range, enum, unique) 확인

### C2. 연결 API

- `GET /v1/calendar/connection`(본인 전용), `PUT /v1/calendar/connection`(source 목록·time zone·sync device 갱신, expected version)
- 권한 상태 전이 검증(설계 §5.2), `revoked`/`denied` 전환 시 projection 즉시 비노출
- 완료 기준: 계약 테스트, 다른 사용자 접근 불가

### C3. Snapshot API와 complete

- `POST /v1/calendar/snapshots`, `PUT .../pages/{n}`, `POST .../complete`, `POST .../abort`
- allowlist 검증(알 수 없는 필드 거부, 개수·크기 상한, window 범위, start<end), 활성 sync device만 허용
- complete: page 수 검증 → 새 generation으로 facts 교체 → projection diff → sync change → `ready`. 같은 revision 재호출은 같은 결과, 실패·중단은 기존 generation 유지
- 요청 제한: 사용자당 snapshot 생성·업로드 제한(`ratelimit`)
- 완료 기준: 원자적 교체, 중간 실패 시 기존 facts 유지, 재호출 멱등, 로그·sync에 시간 구간·source key 원문 없음

### C4. Party 연동

- `createBusyOnlyProjections` stub을 활성 generation facts에서 만들도록 구현(T1, T3)
- 탈퇴·강퇴·해산(P5)에서 projection 삭제는 P5가 소유
- 완료 기준: 캘린더가 ready인 사용자가 Party를 만들고/가입하면 그 즉시 projection이 생긴다

### C5. Freshness와 정리

- `Freshness(ctx, userIDs)`: source_status ready이고 last_completed_sync ≤ 6시간
- staging 24시간 정리 scheduler 작업, window 밖 facts는 complete 뒤 삭제
- 완료 기준: 정리 테스트, stale 판정 테스트

## 브랜치

`feature/calendar-sync-server`. 공개 수준(V1~V4)은 `feature/party-visibility`에서 이어서 한다.
