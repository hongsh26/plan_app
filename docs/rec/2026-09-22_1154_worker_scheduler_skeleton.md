# worker·scheduler 골격 구현

- 날짜: 2026-09-22
- 브랜치: `feature/worker-scheduler`
- 커밋: `b47947e` (구현), 그 뒤 리뷰 반영 커밋
- 구현 계획: `.omc/plans/account-backend-implementation.md` 7단계
- 기준 설계: `docs/account_backend_design.md` §9(프로세스), §12(단일 leader scheduler), §14

## 구현한 것

### `internal/platform/jobs` — outbox 소비

- 점유: `FOR UPDATE SKIP LOCKED` + `locked_until` lease. worker가 죽어 `running`인 채 lease가 지난 row도 다시 점유한다
- **`attempt_count`는 점유할 때 올린다.** 실패할 때 올리면 worker를 죽이는 job(poison pill)은 시도가 기록되지 않아 dead에 도달하지 못한다
- 완료 기록(`Succeed`/실패/dead/반납)은 `(id, status='running', attempt_count)`로 펜싱한다. lease가 지나 다른 worker가 다시 점유했다면 늦게 끝난 앞 시도의 기록은 `ErrLeaseLost`로 무시된다
- 재시도: 상한 `min(30s·2^(attempt-1), 1h)`의 `[d/2, d]` jitter. 절반을 바닥으로 둬 연속 0 근처 재시도를 막는다
- dead: `Permanent(err)` 또는 종류별 `MaxAttempts`(기본 10) 도달. `OnDead` 훅은 dead 전이 트랜잭션 안에서 불린다(§9 "사용자 조치 상태를 sync feed에 기록"의 자리). 훅이 실패하면 dead 전이도 롤백된다
- 등록된 종류만 점유한다. 롤링 배포 중 옛 worker가 새 종류를 dead로 보내 작업을 잃지 않게 하기 위해서다. 끝내 처리되지 않는 종류는 정체 감시가 잡는다
- 종료: 종료 신호 뒤 `ShutdownTimeout`까지 handler를 기다리고, 끝내지 못한 시도는 lease를 반납한다
- `last_error`는 500자로 자르고 UTF-8을 고친다. 민감정보를 넣지 않는 것은 handler 책임이다(§11)
- 아직 등록된 job 종류가 없다. 첫 종류는 계정 삭제다

### `internal/platform/schedule` — 주기 작업 lease

- `scheduled_tasks`(00005)에 작업마다 한 줄을 두고 outbox와 같은 모양으로 점유한다. scheduler가 여럿 떠도 같은 작업은 한 번에 하나만 돈다. 이것으로 §12 "단일 leader"가 결과로 성립한다
- advisory lock을 쓰지 않은 이유: 세션 수준 advisory lock은 연결에 붙는데 pgxpool은 다음 checkout에서 다른 연결을 준다
- 작업 목록은 코드가 소유하고 표는 실행 상태만 가진다. 기동 시 `INSERT ... ON CONFLICT DO NOTHING`

### scheduler 작업 (`cmd/scheduler/tasks.go`)

| 이름 | 주기 | 내용 |
|---|---|---|
| `sync_prune` | 1h | `syncfeed.Prune`(보존 기간 밖). 정리 워터마크 연결 완료 |
| `session_prune` | 1h | 만료된 세션 중 **같은 token family에 살아 있는 세션이 없는 것만** 지운다 |
| `idempotency_prune` | 1h | 만료된 `idempotency_keys` |
| `outbox_prune` | 1h | 7일 지난 `succeeded` job |
| `outbox_stall_check` | 1m | 15분 넘게 점유되지 않은 job이 있으면 `level=ERROR, result=stalled` 로그 |

- 정리 DELETE는 1000개 batch로 나눠 커밋한다. 긴 트랜잭션은 sync settled horizon을 붙잡는다(§12)
- `session_prune`이 family 단위로 보존하는 이유: §5.2는 이미 쓴 refresh token이 다시 오면 family 전체를 폐기하라고 요구한다. 쓴 row를 만료 즉시 지우면 재사용이 "없는 세션"으로 보여 401만 나가고, 살아 있는 후속 세션이 폐기되지 않는다

### 그 밖

- 00005: `scheduled_tasks`, `sessions_expires_at_idx`
- 00006: `outbox_jobs_succeeded_updated_at_idx` (리뷰 반영)
- 설정: `WORKER_LEASE_DURATION`, `WORKER_BATCH_SIZE`
- `internal/platform/apns`: 발송 interface만. 구현은 알림 단계

## 검증

- 로컬: `go vet ./...`, `REQUIRE_DB_TESTS=1 go test -count=1 ./...` 전체 PASS (2026-09-23, 최종 재검증)
- 원격 CI: 최종 feature HEAD `91d50ec`의 push run `35820886698`와 PR run `35821028230` 모두 success. `gofmt`, `go vet`, PostgreSQL 통합 테스트, skip 0, 마이그레이션 왕복과 기동 가드를 통과했다
- 새 테스트가 회귀를 잡는지 확인했다. handler 기한을 `Lease`로 되돌리거나 Permanent 판정을 종료 반납 뒤로 옮기면 해당 테스트가 실패한다

## 독립 리뷰

code-reviewer(opus) 판정: **조건부 MERGE-READY.** High 0, Medium 7, Low 11. 조건은 문서 기록과 M1이었다.

최종 verifier 재검증은 `git diff --check main...HEAD`, worker 주석과 M4 실제 범위, 문서 일관성, jobs 통합 테스트를 다시 확인하고 **MERGE-READY**로 판정했다. PR #2를 rebase 방식으로 2026-09-23 `main`에 병합했다(`f6e6cc1`).

### 반영한 것

| # | 내용 | 반영 |
|---|---|---|
| M1 | 기록이 실패해도 `result=dead`/`success`/`retry` 로그가 나갔다 | `finish`가 커밋 여부를 돌려주고, 결과 로그는 커밋됐을 때만 남긴다. dead 전이 롤백은 `result=dead_failed`(ERROR)로 따로 경보한다. 고치기만 하면 M2가 조용해지므로 경보 대상을 남겼다 |
| M3 | handler 기한이 lease와 같아 기한까지 간 시도의 기록이 항상 lease 뒤였다 | 기한 = 점유 요청 직전 시각 + `Lease - finishTimeout(5s) - 여유(2s)`. `MinLease` 17s(`WORKER_LEASE_DURATION` 검증). scheduler도 같은 모양으로 고치고 `MinTimeout` 17s |
| M6 | `PruneSucceeded`가 index 없이 batch마다 전체 표를 훑었다 | 00006 partial index |
| M7 | 테스트 보강 | 네 결과 기록(Succeed/Retry/Kill/Release) 모두 펜싱 테스트, `StalledCount` 전후 차이와 음성 케이스, 결과 로그 정확성, handler·작업 기한과 lease 관계, worker 역할로 도는 sync prune(batch 1개씩) |
| L1 | 종료 중 Permanent가 반납되어 시도가 되돌려졌다 | Permanent 판정을 반납보다 먼저 본다 |
| L2 | 실제 종료 시간이 `SHUTDOWN_TIMEOUT`을 넘었다 | handler를 `ShutdownTimeout - finishTimeout`에 끊는다. `Release` 주석 정정 |
| L5 | scheduler가 lease를 잃어도 완료 로그를 남겼다 | `result=lease_lost` warn. 늦은 기록이 펜싱되는 테스트 추가 |

### 미룬 것 (계정 삭제 job 등록 전에 결정)

- **M2** OnDead가 계속 실패하면 dead 전이가 롤백되고 job이 다시 점유되어 handler가 다시 돈다(attempt ≤ max인 동안). max를 넘으면 lease마다 Kill 롤백이 되풀이된다. `dead_failed` 경보는 이제 나가지만 되풀이 자체는 막지 않는다. 방향 후보: Permanent 판정을 별도 열로 기록해 다시 점유될 때 handler를 건너뛰고 dead 전이만 backoff로 재시도한다. **OnDead를 가진 첫 종류와 함께 결정한다.** 지금은 OnDead를 가진 종류가 없어 발현되지 않는다
- **M4** batch 전체가 끝나야 다음 점유를 한다. 느린 job 하나가 나머지 슬롯을 lease 한도까지 묶는다(§12 5초 대상화 위반). 세마포어 + 빈 슬롯만큼 점유하는 연속 루프로 바꾼다. 등록된 종류가 없어 지금은 발현되지 않는다
- **M5** `session_prune`은 family에 살아 있는 세션이 있는 한 옛 row를 모두 남긴다. 활성 기기는 회전마다 row가 쌓여 끝없이 는다. **지금 돌고 있는 경로다.** 보존 상한(`used_at < now - N일`은 family가 살아 있어도 삭제)을 두고 "N일을 넘긴 token의 재사용은 탐지하지 않는다"를 설계 §5.2에 적어야 한다. N 결정이 필요하다
- L3 종료가 잦으면 오래 걸리는 job이 dead에 도달하지 못한다(반납이 시도를 되돌린다). 반납 횟수 상한
- L4 scheduler가 작업을 순차로 돌려 sync_prune(최대 10분) 동안 1분 주기 정체 감시가 늦어진다
- L6 sync prune batch에 ORDER BY가 없어 첫 batch에서 워터마크가 크게 뛸 수 있다. 정확성은 맞다
- L7 운영 데이터가 생긴 뒤 추가하는 index는 goose `NO TRANSACTION` 지시어 + `CONCURRENTLY`로 만든다(00006 주석에 적었다). 주석 줄에 goose 지시어 표기를 그대로 쓰면 지시어로 파싱되어 마이그레이션이 실패한다(CI run `35682068409`에서 한 번 실패)
- L8 api 역할도 default privileges로 `scheduled_tasks` DML을 받는다. 마이그레이션은 역할 이름을 쓰지 않는 규약이라 배포 단계에서 역할 권한 방식과 함께 정한다
- L9 `WORKER_BATCH_SIZE`(최대 100)와 `DATABASE_MAX_CONNS`(기본 10)의 관계를 검증하지 않는다
- L10 구현 계획 7단계의 "EventKit 쓰기를 기기 실행 서버 명령으로 모델링"과 §9 scheduler 역할(만료 초대, 알림 예약, 장기 작업 복구, 삭제 시작)은 해당 기능 단계에서 들어온다
