-- sync 변경 피드의 정리 워터마크. account_backend_design.md §7.1.
--
-- 정리 작업은 보존 기간(30일, created_at 기준)이 지난 sync_changes row를 지우고,
-- 지운 row 중 가장 뒤의 (txid, ordinal)을 같은 트랜잭션에서 여기에 기록한다.
-- 읽기는 cursor가 이 위치 이하이면 410 sync_cursor_expired를 돌려준다.
--
-- 시각이 아니라 위치로 판정하는 이유: created_at은 트랜잭션 시작 시각이고 txid는
-- 첫 쓰기 때 배정되므로 두 순서가 어긋난다. 시각으로 판정하면 "하루 넘게 열린
-- 트랜잭션이 없다" 같은 가정이 필요한데, DB 설정으로 정확히 강제하기 어렵다.
-- 위치로 판정하면 가정이 없다. 지운 row는 모두 워터마크 이하이므로, 워터마크보다
-- 뒤의 cursor는 지워진 row를 하나도 건너뛰지 않았다. 워터마크 이하의 cursor는
-- 건너뛰었을 수 있으므로 거부한다(보수적이다).
--
-- 한 줄짜리 표다. singleton 열이 두 번째 줄을 막는다.

-- +goose Up
CREATE TABLE sync_prune_state (
    singleton              boolean     PRIMARY KEY DEFAULT true,
    pruned_through_txid    xid8        NOT NULL DEFAULT '0',
    pruned_through_ordinal int         NOT NULL DEFAULT -1,
    pruned_at              timestamptz,
    CONSTRAINT sync_prune_state_singleton_check CHECK (singleton)
);

INSERT INTO sync_prune_state DEFAULT VALUES;

-- +goose Down
DROP TABLE IF EXISTS sync_prune_state;
