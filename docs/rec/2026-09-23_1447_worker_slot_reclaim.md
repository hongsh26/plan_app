# worker 빈 슬롯 연속 점유 구현

- 브랜치: `feature/account-deletion`
- 범위: worker M4 지적 수정

## 변경

- `Runner.Run`이 batch 전체 완료를 기다린 뒤 다음 claim을 하던 구조를, `BatchSize` 동시성 상한 안에서 빈 슬롯만큼 계속 claim하는 루프로 바꿨다.
- `RunOnce`는 기존처럼 batch 하나를 점유하고 모두 처리한 뒤 반환한다. 기존 테스트와 수동 실행 의미를 유지하기 위해 분리했다.
- 각 claim 직전에 handler deadline을 계산한다. 새로 점유한 job은 해당 claim 기준의 deadline을 공유한다.
- 종료 context가 닫히면 새 claim을 하지 않고, 이미 실행 중인 handler만 기존 grace/release 규칙대로 정리한다.

## 회귀 테스트

- `TestRunnerReclaimsVacantSlotsBeforeSlowJobFinishes`를 추가했다.
- 느린 job 1개와 빠른 job 3개, `BatchSize=2`, 긴 `PollInterval` 조건에서 느린 job이 끝나기 전 빠른 job 3개가 모두 시작되는지 확인한다.
- 동시에 실행된 handler 수가 `BatchSize`를 넘지 않는지도 확인한다.

## 검증

- `env GOCACHE=/private/tmp/plan_app_gocache go test ./internal/platform/jobs` → PASS
- `env GOCACHE=/private/tmp/plan_app_gocache go test ./...` → PASS
- `git diff --check -- server/internal/platform/jobs/worker.go server/internal/platform/jobs/worker_slots_test.go` → PASS

## 남은 사항

- M2의 OnDead 반복 실패 정책은 아직 남아 있다.
- 작업 중 이미 존재하던 계정 삭제·세션 보존 관련 변경은 건드리지 않았다.
