# Party P4 소유권 위임

설계 §5.5 T4, `POST /v1/parties/{id}/owner`(If-Match 필수, 본문 `{"user_id":"..."}`). 브랜치 `feature/party-owner-transfer`.

## 구현

- `internal/party/transfer.go`. 잠금 순서는 `parties` → 두 멤버십(id 순) → `party_invites`(id 순)다. 초대 수락(T3, users→parties→invites)과 교착하지 않는다.
- 기존 방장을 먼저 `member`로 내리고 대상을 `owner`로 올린 뒤 `parties.owner_membership_id`를 바꾸고 version을 올린다. 활성 초대는 모두 `revoked`.
- sync: 활성 멤버 전원에게 party와 두 멤버십 upsert, 초대 tombstone은 이전 방장과 새 방장에게만. outbox `notify_owner_transferred`(dedupe `party_id:party_version`, payload는 식별자만).
- 응답은 위임 직후의 Party(요청자는 이제 일반 멤버라 멤버 기준으로 읽는다), ETag는 새 version.

## 설계와 다르게 한 결정

- 재전송(같은 Idempotency-Key)은 이전 방장이 더 이상 권한이 없어 `403`이다. 설계 §9.4의 "재사용 시에도 현재 권한을 먼저 평가한다"를 그대로 따른 결과이며 성공 payload를 다시 주지 않는다.
- 비활성·비멤버 대상은 `400 invalid_request`(설계 §10). 처음 구현은 404였고 체크리스트 대조에서 고쳤다.

## 검증

- 실제 PostgreSQL로 통과(재검증 뒤 떠난 멤버십 대상 400, 만료됐지만 active인 초대 revoke, 위임·수락 상관 단언을 추가): 역할 교환·초대 revoke·sync 수신자 범위·job dedupe, 일반 멤버 403/비멤버 404/자기 자신·잘못된 ID·비활성 대상 400/낡은 If-Match 409/If-Match 없음 400, 재전송 403과 새 방장의 재위임, 해산 Party 410, 동시 위임 2건(정확히 1건 성공, 방장 1명, 교착 재시도 0), 위임과 초대 수락 경쟁.
- 경쟁 테스트(동시 위임, 위임과 수락)는 `go test -race -count=30 -run 'TestConcurrentTransfers|TestTransferRacing'`에서 안정적이었고, 독립 재검증도 전체 위임 테스트를 `-race -count=10`으로 통과시켰다.

## 남은 것

- `notify_owner_transferred`를 처리하는 worker kind는 설계 9 이후. job은 pending으로 쌓이고 정체 경보 대상이다.
- 계정 삭제 시 강제 위임(`notify_owner_transferred_by_deletion`)은 P8.
