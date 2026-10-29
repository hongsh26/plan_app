# 계정 삭제 연동 (설계 4 P8, 체크리스트 10)

브랜치 `feature/account-deletion-p8`. progressing.md의 "계정 삭제와 Party의 공백"을 메운다.

## 기능 구현 완료

- **강제 위임·해산·종료 (§10 2단계, 설계 4 §5.8):** `party.EndMembershipsForDeletedUser`(`server/internal/party/account_deletion.go`)를
  추가했다. 삭제 worker의 `markDeleting` 트랜잭션이 상태를 `deleting`으로 바꾼 뒤, Apple revoke보다 먼저 부른다. 활성 멤버십을
  `party_id` 순으로 처리한다.
  - 방장이고 승계자가 있으면 T4 코어 `transferOwnerTx`로 강제 위임한다. 알림은 `notify_owner_transferred_by_deletion`이고
    dedupe는 `party_id:party_version`이다. 활성 초대는 무효화된다. 그 뒤 T5 코어 `endMembershipTx`로 `left`/`account_deleted` 종료한다.
  - 방장인데 승계자가 없으면 T7 코어 `disbandPartyTx`(leaver 없음)로 해산한다. 멤버십이 `removed`/`party_disbanded`로 끝나므로
    종료 단계는 다시 실행하지 않는다.
  - 일반 멤버는 `endMembershipTx`로 `left`/`account_deleted` 종료한다. 알림은 `notify_member_left`다(§9.3, T5와 같다).
  - 승계자 규칙은 계획 P8을 따른다. 삭제 대상 본인과 `users.status <> 'active'`인 사용자를 빼고, 활성 멤버를 `joined_at`,
    `user_id` 순으로 정렬해 첫 번째를 고른다.
  - 도메인 코어를 재사용하므로 sync tombstone·upsert, projection·공개 설정 삭제, 초대 tombstone(방장에게만), outbox job이 사용자의
    탈퇴·위임·해산과 같다. 원시 DELETE는 쓰지 않았다.
- **T4 코어 분리:** `transferOwner` handler의 본문을 `transferOwnerTx(…, jobType, now)`로 옮겼다. handler 동작은 그대로다.
- **개인 데이터 삭제 (§10 4단계, §5.8 4):** `finish` 트랜잭션의 `deletePersonalData`가 지운다.
  - `calendar_connections`를 지운다. source 선택, snapshot session(page·staging), busy facts는 FK cascade로 함께 지워진다.
  - 그다음 `party.RebuildBusyProjections`로 projection을 다시 맞춘다(연결 해제와 같은 경로). 멤버십이 이미 끝나 보통 지울 것이
    없지만, 남은 활성 멤버십이 있으면 같은 Party 멤버에게 tombstone이 간다.
  - `availability_preferences`를 지운다. 요일별 구간은 cascade로 지워진다.
  - `users` row는 물리 삭제하지 않으므로 FK cascade에 맡길 수 없었다(rec `2026-10-28_1404`의 P8 차단 항목).
- **삭제 뒤 쓰기 경합 차단:** `Authenticate`는 트랜잭션 밖에서 계정 상태를 본다. 그래서 삭제 요청 직전에 인증을 통과한 요청이
  삭제 정리 뒤에 커밋해 데이터를 되살릴 수 있었다(활동 시간 PUT, 캘린더 연결 PUT, page 업로드).
  - `mutation.Run`이 idempotency 점유 뒤, 재전송 판정과 fn보다 먼저 요청자 `users` row를 잠그고 상태를 다시 본다. active가
    아니면 `mutation.ErrAccountLocked`(= `auth.ErrAccountLocked`, 423 `account_locked`)다. 모든 mutation이 한 곳에서 막힌다.
  - `mutation.Run`을 거치지 않는 snapshot page 업로드(`putPage`)는 트랜잭션 맨 앞에서 `mutation.LockActiveUser`를 부른다.
  - 삭제 요청(`deleteMe`)과 삭제 worker는 같은 row를 `FOR NO KEY UPDATE`로 잡는다. 그래서 어느 쪽이 먼저 커밋하든 요청은 바뀐
    상태를 본다.
- **worker 역할 권한:** 새로 건드리는 표(parties, party_memberships, party_invites, party_visibility_settings,
  party_schedule_projections, calendar_*, availability_*, outbox_jobs, sync_changes)는 `docker/postgres-init/01-roles.sql`의
  default privileges가 이미 worker에 DML을 준다. migration을 추가하지 않았다. 아래 테스트가 worker 역할 pool로 삭제 job을 돌려
  이를 확인한다.

## 설계 또는 구현 방향 확정

- **users 잠금 모드는 기본 `FOR SHARE`이고, 필요한 mutation만 `FOR NO KEY UPDATE`다** (`mutation.Request.LockUserForUpdate`).
  - fn 안에서 users row를 `FOR NO KEY UPDATE`로 다시 잡는 mutation이 있다. 계정 mutation 전부(표시 이름 변경, 삭제 요청)와
    Party 생성·초대 수락(`lockUserForPartyCreate`)이다.
  - 이 mutation이 SHARE로 먼저 잡으면, 같은 사용자의 두 요청이 서로의 업그레이드를 기다리며 교착한다(40P01). 그래서 이들은
    처음부터 NO KEY UPDATE로 잡는다. account의 `run`은 항상 켜고, party는 `runLockingUser`로 켠다.
  - **새 mutation 규칙:** fn 안에서 users row를 `FOR NO KEY UPDATE`로 잡거나 UPDATE하면 `LockUserForUpdate`를 켠다.
    `mutation.Retries()`가 0이 아니면 이 규칙이 깨졌다는 뜻이다.
- **잠금 순서:** worker는 삭제 대상 `users`(NO KEY UPDATE) → `parties`(FOR UPDATE, id 순) → 멤버십(id 순 FOR UPDATE) → 초대 순으로
  잡는다. 승계자 후보의 users row는 잠그지 않는다. parties 뒤에 다른 사용자의 users를 잡으면 users → parties 순인 T3와 교착한다.
- **§10 순서:** 멤버십 정리(2단계)는 revoke 전 `markDeleting`에서 하고, 캘린더·활동 시간(4단계)은 revoke 뒤 `finish`에서 한다.
  revoke가 실패해 재시도하면 이미 끝난 멤버십은 건너뛴다. Apple identity가 없는 계정(개발용 로그인)도 정리를 커밋하도록, identity
  조회의 no rows를 트랜잭션 안에서 처리하게 바꿨다. 예전에는 롤백했지만 상태 변경만 잃어 무해했다.

## 설계 대비 편차

- **승계자 판정 직후의 경합:** 후보를 잠그지 않으므로, 판정 직후 그 후보가 삭제를 요청하면 삭제 중인 사용자에게 넘어갈 수 있다.
  그 사용자의 삭제 작업이 같은 규칙으로 다시 위임하거나 해산하므로 결국 정합하다. §5.8이 막으려던 "연쇄"는 이 좁은 창에서만 생긴다.
- **해산 알림 수신자:** 승계자가 없는 해산은 `notify_party_disbanded`를 enqueue한다. 수신자는 해산 직전 활성 멤버로, 모두 삭제
  진행 중인 사용자다. 알림 worker(설계 9)가 비활성 사용자를 걸러야 한다.
- **기존 테스트 변경:** `TestAccountDeletionAnonymizesNameForCoMembers`를 지웠다. 삭제된 사용자의 멤버십이 이제 tombstone되므로
  같은 Party 멤버의 모델에 이름이 남지 않는다. 이 동작은 새 테스트 `TestAccountDeletionEndsMemberMembershipAndPersonalData`가 본다.
  동작이 바뀌었기 때문이지, 테스트를 맞추려고 고친 것이 아니다.
- `notification_ref_keys`(사용자별 알림 참조 키)는 §10 목록에 없어 이번에 지우지 않았다. 알림 구현(설계 9) 때 삭제 단계에 넣을지 정한다.

## 테스트

`internal/syncfeed/account_deletion_test.go`(삭제 worker는 worker 역할 pool로 돈다):
- `TestAccountDeletionEndsMemberMembershipAndPersonalData`: 일반 멤버 삭제. 멤버십 `left`/`account_deleted`. 같은 Party 멤버는 멤버십과
  projection tombstone을 받아 증분 == bootstrap이 된다. projection·공개 설정·캘린더·facts·활동 시간이 0행이다.
  `notify_member_left`가 1건이고, 재실행해도 결과가 같다.
- `TestAccountDeletionTransfersOwnershipToEarliestActiveMember`: 가장 먼저 가입한 멤버는 삭제 요청 상태라 건너뛰고 B가 방장이 된다.
  Party는 active이고 활성 방장은 1명이다(P8 완료 기준). 초대는 무효화되고 by_deletion 알림이 1건이다. 이어 E의 삭제가 돌아도 방장은 1명이다.
- `TestAccountDeletionDisbandsWhenNoActiveSuccessor`: 남은 멤버가 삭제 요청 중인 Party와 단독 방장 Party가 해산된다. 뒤이은 멤버
  삭제는 `removed`/`party_disbanded`를 바꾸지 않는다.
- `TestRequestAuthenticatedBeforeDeletionCannotWriteAfterIt`(availability·calendar·party): 기기 잠금으로 요청을 idempotency INSERT에서
  멈춘 뒤 계정 상태를 바꾼다. 요청은 423이고 아무것도 쓰지 않으며 재시도도 0이다. 상태 확인을 끄면 availability와 calendar가
  200으로 쓴다는 것을 확인했다.
- `internal/calendar/deletion_race_test.go` `TestSnapshotPageWaitingOnDeletionRequestIsRejected`: 삭제 요청 트랜잭션이 users row를 쥔
  동안 page 요청이 그 잠금에서 기다리게 한다. 커밋 뒤 423이고 staging 0행이다. 확인을 끄면 200이다.
- `TestPartySyncIncrementalMatchesBootstrap`에 11단계(방장 D 삭제 → A 강제 위임, 모든 세션 증분 == bootstrap)와 12단계(승계자 없는
  방장 E 삭제 → 해산, 기존 세션 불변)를 더했다. Swift fixture `sync_sequential_acceptance.json`을 다시 기록했다(다른 fixture는 그대로).
- 게이트: gofmt·vet, `migrate reset → up`, `go test -count=1 ./...` 605 pass, skip 0. iOS `xcodegen` + `xcodebuild build test` 성공.
  - `migrate reset` 직후 첫 실행에서 `ratelimit` `TestExceededIsReportedOnceAndCacheDoesNotHideIt`가 한 번 실패했다("요청 제한을
    확인할 수 없다"). 단독으로 3회, 전체로 다시 돌리면 통과했다. 이번 변경과 무관한 간헐 실패로 보인다.

## 남은 것

- 설계 7(제안·응답·확정)이 들어오면 삭제 단계에 제안 처리를 더한다. 진행 중 제안의 `membership_ended` 신호와 확정 기록 익명화
  (§10 5단계)다. `endMembershipTx`·`disbandPartyTx`의 TODO와 같은 자리다.
- 설계 8: 삭제 시 캘린더 쓰기 command·locator 삭제와 참여자 `released`(`account_deleted`)다.
- 설계 9: `notify_owner_transferred_by_deletion` worker kind와 비활성 수신자 필터다.
- iOS: 삭제된 멤버의 멤버십이 tombstone으로 사라지는 것을 화면이 자연스럽게 처리하는지 E2E(14번)에서 본다.
