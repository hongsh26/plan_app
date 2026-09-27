# 계정 삭제 API와 첫 account_deletion job 구현

- 작업 시각: 2026-09-23 14:55 KST
- 소요: 약 35분
- 유형: 기능 구현 완료, 검증 완료, 구현 방향 확정

## 완료

- `DELETE /v1/me`를 추가했다. `Idempotency-Key`와 `If-Match`가 필수이고 body는 비어 있어야 한다.
- 삭제 요청 mutation은 한 트랜잭션에서 `users`를 `FOR NO KEY UPDATE`로, 모든 `devices`를 안정된 순서로 `FOR NO KEY UPDATE`로 잠근다.
- 같은 트랜잭션에서 모든 session revoke, 모든 device push token 제거와 revoke/version 증가, user `deletion_requested` 전환/version 증가, user/device sync projection, audit, `account_deletion` dedupe job enqueue를 수행한다.
- 성공 응답은 `202 Accepted`와 `user.status=deletion_requested`다. 성공 후 재전송은 세션/기기가 이미 폐기되므로 idempotency replay가 아니라 인증 계층에서 `401 invalid_session` 또는 `423 account_locked`로 차단되는 계약으로 문서화했다.
- worker에 첫 job kind `account_deletion`을 등록했다. payload는 `user_id`만 엄격 파싱한다.
- 삭제 worker는 DB 잠금 밖에서 Apple refresh token revoke를 호출하고, 현재 구현된 데이터에서 멱등적으로 `deletion_requested -> deleting -> deleted`로 수렴시킨다.
- Apple `invalid_grant`는 이미 revoke된 token으로 보고 성공 처리한다. 그 밖의 Apple 오류(`invalid_client` 등)와 revoker 설정 누락은 설정 복구 후 재시도할 수 있도록 retryable로 처리한다. 작업 종류별 최대 시도 횟수에 도달하면 dead 경보와 운영 복구가 필요하다.
- 완료 트랜잭션에서 Apple identity row를 물리 삭제하고, session/device를 정리하며, audit actor를 `deleted_user:<hash>` tombstone으로 치환한다. scheduler는 익명 tombstone을 90일 뒤 batch 삭제한다.
- `deletion_requested_at`을 별도 열에 기록해 24시간 완료 목표를 계정 생성 시각이나 변경 가능한 `updated_at`이 아니라 실제 요청 시각에서 측정한다.
- 24시간 완료 목표를 넘겨도 삭제 worker는 계속 진행한다. 지연은 운영 경보 대상이며 삭제를 막는 조건이 아니다.

## 범위 밖

- 아직 Party, calendar connection, proposal, confirmed event 테이블이 없으므로 §10의 해당 데이터 정리와 협업 기록 익명화는 구현 대상이 아니었다. 후속 도메인 테이블 구현 시 account deletion worker에 단계를 추가해야 한다.
- 배포 환경 KMS는 아직 없다. worker도 local secretbox만 조립하며 `APP_ENV!=local`에서는 기존 KMS 미구현 가드로 기동하지 않는다.
- 현재 `account_deletion` job은 `OnDead`를 쓰지 않는다. M2는 별도로 `dead_pending` finalization 상태를 추가해 후속 OnDead job도 안전하게 만들었다.

## 검증

- `go test ./internal/account ./internal/platform/config ./cmd/worker`
- `go test ./...` (샌드박스의 로컬 포트 listen 제한 때문에 권한 상승 후 재실행)

## 변경 파일

- `server/internal/account/deletion.go`
- `server/internal/account/deletion_test.go`
- `server/internal/account/account.go`
- `server/internal/account/account_test.go`
- `server/internal/platform/config/auth.go`
- `server/internal/platform/config/auth_test.go`
- `server/cmd/worker/main.go`
- `server/openapi/openapi.yaml`
- `docs/progressing.md`
