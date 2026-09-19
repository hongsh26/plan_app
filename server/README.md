# PlanTogether 서버

Go + PostgreSQL 모듈러 모놀리스. 기준 설계는 [`docs/account_backend_design.md`](../docs/account_backend_design.md)다.

현재 구현 범위는 **P0 서버 골격**이다. 세 진입점, 설정 검증, health/readiness,
8개 테이블 마이그레이션까지다. 인증과 도메인 endpoint는 P1 이후다.

## 구성

| 경로 | 역할 |
|---|---|
| `cmd/api/` | HTTP API. 인증·권한·상태 전이·sync (§9). P0에서는 health endpoint와 마이그레이션 실행만 |
| `cmd/worker/` | APNs 발송, 계정 삭제, 명령 만료·재할당 (§9). P0에서는 no-op 루프 |
| `cmd/scheduler/` | 만료 초대, 알림 예약, 작업 복구 (§9). P0에서는 no-op 루프 |
| `internal/platform/config/` | 환경 변수 로드와 기동 시 검증 |
| `internal/platform/postgres/` | pgx 연결 pool과 goose 마이그레이션 러너 |
| `internal/platform/httpapi/` | `net/http` 서버 골격, health/readiness 핸들러, 구조화 로거 |
| `migrations/` | SQL 마이그레이션. 바이너리에 embed된다 |
| `openapi/` | API 계약. P0 범위인 health endpoint만 |
| `test/` | PostgreSQL 통합 테스트 (§8 불변식 검증) |

도메인 패키지(`internal/auth/`, `internal/account/`, `internal/sync/` 등)는 §13에
있지만 P0에서 만들지 않는다. 빈 디렉터리를 두지 않는다.

## 사전 요구

- Go 1.27 이상
- Docker와 Docker Compose

goose 바이너리는 **필요 없다.** goose를 라이브러리로 쓰고 마이그레이션을
`embed.FS`로 바이너리에 담는다.

## 실행 순서

### 1. PostgreSQL 기동

```sh
cd server
docker compose up -d
```

컨테이너 첫 기동 시 `docker/postgres-init/01-roles.sql`이 §11의 DB 역할 4개를
만든다: `plantogether_migration`(DDL 전용), `plantogether_api`,
`plantogether_worker`, `plantogether_readonly`.

준비될 때까지 기다리려면:

```sh
docker compose ps        # db가 healthy인지 확인
```

### 2. 환경 변수 설정

```sh
cp .env.example .env
```

`.env`는 `.gitignore`에 있어 커밋되지 않는다. `.env.example`만 추적한다.

셸에 불러오려면:

```sh
set -a && . ./.env && set +a
```

### 3. 마이그레이션 적용

```sh
go run ./cmd/api -migrate up
```

`MIGRATION_DATABASE_URL`과 `APP_ENV`를 쓴다. 런타임 `DATABASE_URL`(api 역할)은
DDL 권한이 없으므로 §11의 역할 분리가 유지된다.

다른 방향:

```sh
go run ./cmd/api -migrate status   # 적용 상태
go run ./cmd/api -migrate down     # 마지막 하나 되돌리기 (APP_ENV=local 전용)
go run ./cmd/api -migrate reset    # 전부 되돌리기 (APP_ENV=local 전용)
```

`down`과 `reset`은 `APP_ENV=local`에서만 실행된다. 다른 값이거나 `APP_ENV`가
없으면 `postgres.Migrate`가 DB에 연결하기 전에 거부한다. 프로덕션 api task에
`MIGRATION_DATABASE_URL`이 주입된 상태에서 `-migrate reset`이 한 번 실행되면
스키마 전체가 사라지기 때문이다. 현재 마이그레이션은 하나뿐이라 `down` 한 번이
`reset`과 결과가 같으므로 둘 다 막는다.

### 4. 서버 실행

```sh
go run ./cmd/api          # 기본 :8080
go run ./cmd/worker
go run ./cmd/scheduler
```

세 진입점 모두 SIGINT/SIGTERM에 graceful shutdown한다.

### 5. health 확인

```sh
curl -i localhost:8080/livez    # 200
curl -i localhost:8080/readyz   # 200
```

DB를 내리면 readiness만 503이 된다:

```sh
docker compose stop db
curl -i localhost:8080/livez    # 200  (DB에 의존하지 않는다)
curl -i localhost:8080/readyz   # 503
docker compose start db
```

liveness가 DB에 의존하면 DB 장애가 모든 API 인스턴스의 재시작 루프로 번진다.
그래서 두 endpoint를 나눴다.

## 테스트

### 단위 테스트 (DB 불필요)

```sh
go test ./internal/...
```

### PostgreSQL 통합 테스트

§8의 필수 불변식을 DB가 실제로 거부하는지 확인한다.

```sh
TEST_DATABASE_URL='postgres://plantogether_migration:local_migration_password@localhost:5432/plantogether?sslmode=disable' \
  go test ./test/...
```

`TEST_DATABASE_URL`이 없으면 **skip**한다. skip이 통과로 위장되지 않게 하려면
`REQUIRE_DB_TESTS=1`을 켠다. 그러면 DB에 연결할 수 없을 때 skip 대신 실패한다.

```sh
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL='...' go test ./test/...
```

## CI

`.github/workflows/server-ci.yml`이 `server/` 변경마다 돈다. 이 워크플로가
`REQUIRE_DB_TESTS=1`을 켜는 주체다. 켜는 주체가 없으면 `go test ./...`는 통합
테스트를 전부 skip하고 `ok`를 출력하므로, §8 불변식 검증이 통째로 사라져도
초록불이 난다.

CI는 GitHub Actions의 `services:` 블록이 아니라 `docker compose up -d --wait`을
쓴다. `services:`는 `docker-entrypoint-initdb.d`를 마운트할 수 없어
`docker/postgres-init/01-roles.sql`을 따로 다시 적용해야 하고, 그러면 CI와
로컬의 역할·기본 권한이 갈라진다.

CI가 확인하는 것:

- `gofmt`, `go vet`
- 마이그레이션 `up` -> `status` -> `reset` -> `up` 왕복
- `REQUIRE_DB_TESTS=1`로 통합 테스트 실행, 그리고 skip된 테스트가 **0건**인지
  별도 확인 (`connect()` 헬퍼를 거치지 않는 테스트가 나중에 추가될 경우 대비)
- `APP_ENV=production`에서 `-migrate reset`이 거부되는지 (가드가 진입점까지
  연결돼 있는지)

### 전체

```sh
go build ./... && go vet ./... && go test ./...
```

## 설계상 결정

### 상태 문자열은 enum이 아니라 CHECK constraint

후속 설계(P1~P8)가 계속 허용 값을 추가하므로 `ALTER TYPE` 마찰을 피한다.
모든 CHECK에 이름을 붙여 두었으므로 값을 늘릴 때
`DROP CONSTRAINT <이름>` 후 `ADD CONSTRAINT <이름>`으로 교체한다.

### `version` 열은 클라이언트가 직접 수정하는 row에만

§6이 "수정/삭제는 `If-Match` 또는 `expected_version`을 요구한다"고 하므로
낙관적 동시성 토큰은 §6 endpoint 표에 mutation이 있는 테이블에만 필요하다.
현재 `users`와 `devices`뿐이다. `sync_changes`와 `audit_events`는 append-only라
`version`이 없다. 근거는 `migrations/00001_init.sql` 머리말에 있다.

### sync 순서 키는 `(txid, ordinal)`

`BIGSERIAL`을 쓰지 않는다. `nextval`이 트랜잭션 밖에서 소비되어 번호 순서와
커밋 순서가 어긋나고, 먼저 번호를 받고 나중에 커밋한 트랜잭션의 변경이 영구
유실되기 때문이다. 탈락시킨 대안과 근거는
[`docs/rec/2026-09-18_1719_sync_cursor_ordering_amendment.md`](../docs/rec/2026-09-18_1719_sync_cursor_ordering_amendment.md)에
있다. 읽기 쿼리는 settled horizon(`pg_snapshot_xmin`) 아래만 반환해야 하며,
이 읽기 경로는 P6에서 구현한다.

### 자격 증명

- `.env`는 커밋하지 않는다. `.env.example`만 추적한다.
- `docker-compose.yml`과 `docker/postgres-init/01-roles.sql`의 비밀번호는
  **로컬 전용**이다. 스테이징·프로덕션은 §12에 따라 자격을 분리하고 secret
  manager에서 주입한다. 이 값을 재사용하지 않는다.
- 설정 오류 메시지는 변수 **이름만** 말하고 값을 에코하지 않는다. DSN에
  비밀번호가 들어 있으므로 값을 출력하면 기동 경로에서 §11을 위반한다.
- 구조화 로그는 §11에 따라 `request_id`, actor 내부 ID, action, result,
  latency만 기록한다. token, Apple credential, 캘린더 제목·장소·메모, 초대
  원문을 남기지 않는다.
