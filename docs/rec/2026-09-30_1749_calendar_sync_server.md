# 캘린더 동기화 서버 (시연 트랙 C1~C5)

설계 3의 서버 최소 조각. 계획 `.omc/plans/calendar-sync-server-implementation.md`, 브랜치 `feature/calendar-sync-server`.

## 구현

- **스키마**(migration 00012): `calendar_connections`(사용자당 1, sync device, source_status, generation, last_completed_sync_at, last_snapshot_revision), `calendar_source_selections`, `calendar_snapshot_sessions/pages/facts_staging`, `calendar_busy_facts`. 모두 사용자 소유라 CASCADE. ready는 완료 sync가 있어야 한다는 CHECK로 "권한 없음/미동기화를 일정 없음으로 오인"을 막는다.
- **API**(`internal/calendar`): `GET/PUT /v1/calendar/connection`, snapshot 생성·page 업로드·complete·abort. allowlist(알 수 없는 필드 400), 시간대/시각/window/키 길이 검증, 활성 sync 기기만 업로드, 열린 session은 하나, revision 단조, complete는 page 수·fact 수를 모두 확인 후 facts를 새 generation으로 원자 교체. 같은 session 재complete는 같은 결과.
- **projection 서비스**(`party.RebuildBusyProjections`): 사용자의 활성 Party마다 facts와 diff해 upsert/tombstone만 발행(바뀌지 않으면 sync 변경 없음). hidden은 projection 없음, details는 상세가 없어 busyOnly로 안전 강등. 상태 전이(denied/restricted/revoked/needs_source_reselection/disconnected)는 projection을 같은 트랜잭션에서 즉시 지우고 facts는 유예한다(disconnected는 facts도 삭제). Party 생성(T1)·가입(T3)의 stub을 이 서비스로 대체하고, 가입자에게 다른 멤버 projection을 부트스트랩으로 발행한다.
- `calendar.FreshUsers`(ready이고 6시간 이내), `calendar.Prune`(24시간 지난 session, scheduler `calendar_snapshot_prune`), 요청 제한(`calendar.snapshot` 10/분, `calendar.page` 300/분, 사용자당).
- 시연: `demo_api.sh`에 캘린더 연결·업로드·다른 멤버의 sync 수신·제목 거부(400) 단계 추가.

## 잠금 순서 (교착 방지)

전 트랜잭션이 users → parties → party_memberships → party_invites → calendar_connections 순을 지킨다. T1·T3는 자기 Party를 잠근 뒤 마지막에 connection을 FOR SHARE로 잡고 facts를 읽는다. snapshot complete와 연결 PUT은 같은 순서를 따르려고 사용자가 속한 활성 Party 행을 id 순으로 FOR SHARE로 **먼저** 잡고(`party.LockUserPartiesShared`) connection을 FOR UPDATE로 잡고 멤버십은 FOR KEY SHARE로만 잡는다. 처음에는 Party를 잠그지 않았는데, 재검증이 "가입하는 T3와 겹치면 신규 멤버가 수신자에서 빠지고 부트스트랩이 낡은 projection을 받는다"고 지적했다. 선잠금을 빼고 돌리면 가입자 피드 재구성이 최종 projection과 어긋나는 것을 실제로 재현했다.

## 설계와 다르게 한 결정

- 연결 생성은 `If-Match: "0"`이 아니라 If-Match 없는 create-only PUT이다(공용 파서가 version 0을 거부).
- page당 250 facts(본문 64KB 제한), 최대 200 page, 총 20,000 facts.
- 이번 범위 밖: details ciphertext(공개 수준), writer device·write command, 7일 유예 뒤 facts 삭제 job, sync device 교체의 전체 흐름.

## 테스트 인프라 수정 (이 작업에서 드러난 기존 결함)

`server/test`의 `DROP TRIGGER` 스키마 테스트가 공유 테이블에 ACCESS EXCLUSIVE 잠금을 잡아, 병렬로 도는 다른 패키지 테스트와 교착했다(PostgreSQL이 희생자를 임의로 골라 Party 테스트·정리가 죽었다). 새 projection 쿼리의 테이블 잠금 순서를 기존과 같게(parties → party_memberships) 맞추고, 이 테스트는 `lock_timeout 300ms`로 물러났다가 재시도하게 바꿨다. 전체 스위트 12회 연속 통과.

## 검증

- 실제 PostgreSQL 전체 스위트, skip 0건. 캘린더 패키지: 연결 전이·create-only, snapshot 교체·멱등·빈 snapshot, 불완전/abort 시 기존 generation 유지, 입력 검증(allowlist 포함), source 선택·revision 단조, 소유자·sync 기기 경계, projection 생성·diff·hidden·details 강등·철회·재연결·해제, Party 생성/가입 시점 연동, freshness, prune, 동시성(`-race -count=20`), 로그에 fact 데이터 없음.
- 실제 서버 시연 스크립트 통과.

## 독립 재검증과 반영 (REQUEST_CHANGES → 수정)

- **High: 철회 뒤 늦은 complete가 ready로 되살림** — complete가 `syncing`이 아니면 409(`calendar_state_invalid`), 상태가 syncing을 벗어나는 PUT은 열린 session을 폐기한다(두 겹). 변형 테스트로 가드 제거 시 실패함을 확인했다.
- **Med: disconnect 뒤 abort 500** — abort의 복귀 조건에 `last_completed_sync_at IS NOT NULL`을 추가(없으면 selecting).
- **Med: T3와 complete 경합으로 가입자 피드 유실**, **동시성 테스트가 잠금 순서를 검증하지 못함** — 위 선잠금, 그리고 같은 사용자의 complete가 그 사용자의 Party 생성·연결 있는 사용자의 가입과 겹치는 6라운드 테스트를 추가하고 가입자 피드 재구성 == 최종 projection을 단언한다.
- **Med: 대량 fact의 N+1 발행** — `mutation.Tx.EmitChanges`(unnest 일괄 INSERT, 1000개씩)로 바꿨다. 1500 facts × 3명 테스트 추가.
- Low 반영: PUT의 가시성 변화는 양방향(`oldVisible != newVisible`)에서 projection을 맞춘다, `"Local"` 시간대 거부, 잠금 순서 주석 정정.
- Low 기록만: `revoked_at`이 selecting/error 전이에서 초기화돼 7일 삭제 job(후속)이 이 필드에 의존하면 다시 설계해야 한다. prune이 방치된 open session을 지운 뒤 연결이 syncing으로 남는다(다음 create가 복구). 같은 revision 재create가 abort된 session도 200으로 돌려준다(이후 page 업로드는 409 `calendar_snapshot_state`). staging 누적 상한은 complete에서만 검사(24시간 prune으로 제한). 삭제 후 같은 ID로 재삽입된 projection은 version 1로 시작한다(클라이언트는 피드 순서로 처리).

## 남은 것

- **iOS 연동 전제:** `GET /v1/sync/bootstrap`이 아직 user·기기만 내려준다(Party·멤버십·projection 없음)이고 내 Party 목록 endpoint도 없다. 공개 수준·검색 뒤 P6-lite로 채운다.
- OpenAPI에 Party·초대·캘린더 endpoint 등재.
