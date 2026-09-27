# worker finalization과 보존 상한 확정

- 작업 시각: 2026-09-23 15:21 KST
- 브랜치: `feature/account-deletion`
- 유형: 버그 수정 완료, 검증 완료, 설계 방향 확정

## M2 — dead finalization 분리

- `outbox_jobs`에 `dead_pending` 상태를 추가했다.
- handler의 permanent/최대시도 판정이 끝나면 먼저 `dead_pending`으로 펜싱한다. `OnDead`가 실패하면 handler를 다시 실행하지 않고 finalization만 backoff 뒤 재시도한다.
- `dead_pending`은 active dedupe와 claim/stall 대상이다. 같은 dedupe job이 finalization 중 중복 enqueue되지 않는다.
- 최초 finalization 동안 현재 lease를 유지해 다른 worker가 중간에 attempt를 올리는 race를 막는다. 실패했을 때만 lease를 풀고 다음 시각을 backoff로 미룬다.

## M4 — 빈 worker 슬롯 즉시 재사용

- `Runner.Run`은 `BatchSize - inFlight`만큼 점유한다.
- job 하나가 끝나면 batch의 느린 job을 기다리지 않고 반환된 슬롯만큼 다시 점유한다.
- 종료 뒤에는 새 job을 점유하지 않고 기존 grace/release/fencing 의미를 유지한다.

## M5 — refresh 재사용 탐지 보존

- 사용된 refresh token 해시는 `used_at`부터 90일간 보존한다. 기본 refresh 수명 30일의 3배다.
- 90일이 지나면 활성 token family에 속한 과거 row도 삭제한다. 그 token이 다시 제시되면 family 탈취가 아니라 일반 무효 token으로 처리한다.
- `sessions_used_at_idx`를 새 migration으로 추가했다. 이미 적용된 00005를 수정하지 않았다.

## 검증

- migration 00001~00009 적용 PASS
- `REQUIRE_DB_TESTS=1 go test -count=1 ./...` PASS
- M2: OnDead 실패 rollback, backoff, handler 미재실행, fencing, active dedupe 회귀 테스트
- M4: slow job이 끝나기 전에 빈 슬롯으로 fast job을 계속 처리하고 동시성이 `BatchSize`를 넘지 않는 회귀 테스트
- M5: 활성 family의 90일 이내 row 보존과 90일 초과 row 삭제 회귀 테스트
