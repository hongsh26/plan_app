-- 요청 제한 고정 창 카운터. 근거는 docs/rate_limit_design.md §4.1.
--
-- FK를 두지 않는다. users 등 다른 테이블에 의존하지 않으므로 users -> devices -> sessions
-- 잠금 규약과 users-first 규약에 참여하지 않고, 사용자 삭제가 이 표에 막히지 않는다.
-- key_hash는 HMAC-SHA256(RATE_LIMIT_KEY, scope || 0x00 || subject)이라 원문 IP나 사용자
-- ID가 저장되지 않는다. row는 최대 1시간(가장 긴 창)만 의미가 있고 scheduler의
-- rate_limit_prune이 2시간 지난 row를 지운다.
--
-- 창 경계는 DB 시각(date_bin)으로 정하므로 API task 간 시계 오차가 창을 어긋나게 하지 못한다.
-- date_bin은 PostgreSQL 14 이상이 필요하다.
--
-- 권한은 01-roles.sql의 default privileges를 따른다(api·worker CRUD, readonly SELECT).
-- INSERT ... ON CONFLICT ... RETURNING과 DELETE ... WHERE는 SELECT 권한도 필요하므로
-- 좁히지 않는다.

-- +goose Up
CREATE TABLE rate_limit_buckets (
    scope        text        NOT NULL,
    key_hash     bytea       NOT NULL,
    window_start timestamptz NOT NULL,
    count        integer     NOT NULL,
    PRIMARY KEY (scope, key_hash, window_start),
    CONSTRAINT rate_limit_buckets_count_positive_check CHECK (count > 0)
);

-- 정리 경로: window_start가 오래된 row를 배치로 지운다.
CREATE INDEX rate_limit_buckets_window_start_idx ON rate_limit_buckets (window_start);

-- +goose Down
DROP INDEX IF EXISTS rate_limit_buckets_window_start_idx;
DROP TABLE IF EXISTS rate_limit_buckets;
