# 탈퇴·강퇴·해산과 공개 수준 cleanup 구현 (설계 4 P5 + 설계 5 V3 서버)

## 기능 구현 완료

- `DELETE /v1/parties/{id}/memberships/me`(T5), `DELETE /v1/parties/{id}/memberships/{user_id}`(T6, If-Match),
  `POST /v1/parties/{id}/disband`(T7, If-Match)를 `server/internal/party/membership_end.go`에 추가했다.
- 공용 코어 `endMembershipTx`(T5·T6)와 `disbandPartyTx`(T7, 단독 방장 탈퇴 포함)를 분리했다. P8 계정 삭제 연동이
  `end_reason='account_deleted'`로 재사용할 수 있다.
- 같은 트랜잭션에서 떠나는 사용자의 `party_schedule_projections`(ciphertext 포함)와 `party_visibility_settings`를
  삭제한다. `calendar_busy_facts`는 개인 소유라 남긴다.
- 알림 job `notify_member_left`·`notify_member_removed`(dedupe: membership_id), `notify_party_disbanded`
  (dedupe: `party_id`, §9.3 표)를 enqueue한다. worker kind는 아직 없어 pending으로 쌓인다(기존 join·transfer와 같다).
  단독 방장 탈퇴 경로는 `notify_party_disbanded`를 내지 않는다.
- OpenAPI에 세 endpoint, `PartyEnvelope`·`PartyMembershipEnvelope`, `PartyDisbanded` 응답, 오류 코드
  `party_disbanded`·`owner_transfer_required`를 추가했다.

## 설계 또는 구현 방향 확정

- **증분 sync는 bootstrap과 같아야 한다.** bootstrap은 활성 Party·활성 멤버십만 담으므로 T5 step 8의
  "탈퇴자 멤버십 상태 변경"과 해산 Party도 upsert가 아니라 tombstone으로 보낸다(§9.2와 일치).
- **tombstone version 규칙:**
  - 실제로 끝났거나 삭제된 entity(종료 멤버십, 해산 Party, 삭제한 projection·설정)는 version이 있는
    tombstone이다. projection·설정은 마지막 version+1(하향 삭제 경로와 같다). 멤버십·Party는 전이로 올린 version이다.
  - 서버에 남아 있고 수신자의 scope에서만 빠지는 entity(탈퇴자가 보던 Party, 다른 멤버의 멤버십·projection)는
    version 없는 tombstone이다. 재가입 시 같은 ID·같은 version으로 다시 내려오므로 version 하한을 남기면
    그 upsert가 무시된다.
  - 예외: 초대는 version 열이 없어 무효화돼도 version 없는 tombstone이다(기존 위임 경로와 같다).
  - **iOS 계약:** version 없는 tombstone은 로컬 row만 지우고 version 하한을 기록하지 않는다. OpenAPI `SyncChange.version`에 반영했다.
- **탈퇴 재전송(§9.4):** 이미 비활성이면 마지막 멤버십과 200을 돌려주고 sync를 만들지 않는다. Party가 해산됐으면
  (단독 방장 탈퇴의 재전송 포함) 410 `party_disbanded`다. **iOS 오프라인 큐는 탈퇴의 200과 410을 모두 완료로 처리한다.**
  단, 같은 키 재전송은 저장된 결과를 돌려주므로 다른 기기에서 재가입한 뒤에도 `left` 200이 올 수 있다. **iOS는 탈퇴 응답만으로
  로컬 Party를 지우지 않고 sync tombstone을 근거로 정리한다.**
- 강퇴 대상이 활성 멤버가 아니면 404, 방장 자신이면 400이다. 해산 재전송은 현재 권한 평가에서 410이다.
- 잠금: `parties FOR UPDATE` → 활성 멤버십 id 순 `FOR UPDATE` → 초대. 멤버십 `FOR UPDATE`가 snapshot rebuild의
  `FOR KEY SHARE OF m`과 충돌하므로 종료 중인 멤버십에 projection을 다시 쓸 수 없다.

## 검증 완료

- 새 통합 테스트 8건(`membership_end_test.go`): 탈퇴 양쪽 tombstone·bootstrap 0건·재전송 무변경, 방장 409와
  단독 방장 해산, 강퇴 권한·version·재가입 busyOnly, 해산 410/404 구분과 초대 tombstone 방장 한정,
  탈퇴↔공개 설정 경쟁, 탈퇴↔snapshot rebuild 경쟁(projection 부활 없음), 해산↔초대 수락 교착 없음.
  경쟁 테스트 3건은 `mutation.Retries()` 증가 0을 확인한다(Run이 40P01을 재시도로 숨기므로).
- 동시 실행 테스트는 interleaving을 강제하지 못하므로 `TestLeaveWaitsForSnapshotLocks`를 더했다. 수동 트랜잭션이
  `parties FOR SHARE` 또는 멤버십 `FOR KEY SHARE`를 잡은 동안 탈퇴가 `pg_stat_activity`에서 Lock 대기인지 확인한다.
  멤버십 잠금을 `FOR NO KEY UPDATE`로 약화하면 이 테스트가 실패하는 것을 직접 확인했다.
- 코드 리뷰 서브 에이전트 재검증: High 0. Medium 2(경쟁 테스트의 증명력)와 Low(강퇴 user_id 파싱 순서, 해산 sync 일괄
  메모리, dedupe key, OpenAPI 400·version 설명, 수신자 단언 보강, 문서)를 반영했다.
- `REQUIRE_DB_TESTS=1 go test -p 1 ./... -count=1` 전체 통과(Docker Compose PostgreSQL, schema 15), `go vet`, `gofmt`,
  OpenAPI YAML 파싱, `git diff --check` 통과.

## 남은 작업

- 설계 7·8 테이블이 생기면 T5·T7에 추가: `membership_ended`·`party_disbanded` 제안 신호, `proposal` tombstone,
  `calendar_cleanup_suggestions`, `calendar_write_commands` 취소(코드에 TODO).
- P8 계정 삭제 연동은 두 코어를 재사용해 구현한다.
- iOS: 탈퇴·해산 tombstone 수신 시 Party 캐시 삭제, 로그아웃 캐시 삭제, cursor 만료 scope 교체, 설정 화면(V3 iOS·V5).
