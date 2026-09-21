-- scheduler 주기 작업의 DB lease. account_backend_design.md §12: "단일 leader
-- scheduler. scheduler 작업 자체는 DB lease로 중복을 방지한다."
--
-- leader 선출을 따로 두지 않는다. 작업마다 한 줄을 두고 outbox_jobs와 같은
-- 모양(FOR UPDATE SKIP LOCKED + locked_until)으로 점유한다. scheduler가 둘 떠도
-- 같은 작업을 동시에 돌리지 않으므로 "단일 leader"가 결과로 성립한다.
--
-- advisory lock을 쓰지 않는 이유: 세션 수준 advisory lock은 연결에 붙는데 pgxpool은
-- 다음 checkout에서 다른 연결을 준다. 잠금이 기대한 곳에 없어도 조용히 지나간다.
--
-- 줄은 scheduler가 기동할 때 이름으로 만든다(INSERT ... ON CONFLICT DO NOTHING).
-- 작업 목록은 코드가 소유하고 이 표는 실행 상태만 가진다.

-- +goose Up
CREATE TABLE scheduled_tasks (
    name         text        PRIMARY KEY,
    next_run_at  timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    last_run_at  timestamptz,
    last_error   text,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- 세션 정리 경로. 정리는 만료된 row 중 같은 token family에 살아 있는 세션이 없는
-- 것만 지운다(internal/auth/cleanup.go).
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- +goose Down
DROP INDEX IF EXISTS sessions_expires_at_idx;
DROP TABLE IF EXISTS scheduled_tasks;
