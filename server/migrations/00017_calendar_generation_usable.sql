-- active generation을 현재 연결 상태에서 가능 시간 계산에 쓸 수 있는지 표시한다
-- (docs/availability_search_design.md §5.1, docs/rec/2026-10-28_1431_availability_search_api.md 결정 6).
--
-- source_status만으로는 철회 전 generation과 정상 재동기화 중인 generation을 구분할 수 없다.
-- ready → revoked → selecting → (snapshot 생성) syncing 경로에서 철회 전 facts가 다시 "재동기화 중"으로 보인다.
--   - snapshot complete로 새 generation이 활성화되면 true
--   - 비노출 상태(denied, restricted, revoked, needs_source_reselection, disconnected, permission_required,
--     selecting)로 가거나 읽기 source 선택이 바뀌면 false. abort는 false면 selecting으로 돌아간다
--   - ready ↔ error, ready·error → syncing(정상 재동기화, 기기 전환)은 유지(마지막 정상 데이터 유지, 설계 3 §5.2)
-- 가능 시간 검색, party projection 가시성, calendar.FreshUsers가 모두 이 열을 요구한다.

-- +goose Up
ALTER TABLE calendar_connections
    ADD COLUMN active_generation_usable boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT calendar_connections_usable_requires_generation_check CHECK (
        NOT active_generation_usable OR active_generation > 0
    );
-- 기존 row: ready인 연결만 쓸 수 있다고 본다. syncing·error 등은 다음 complete까지 false(fail closed)이며
-- 다음 projection 재구성에서 비노출된다.
UPDATE calendar_connections SET active_generation_usable = true
 WHERE source_status = 'ready' AND active_generation > 0;

-- +goose Down
ALTER TABLE calendar_connections
    DROP CONSTRAINT IF EXISTS calendar_connections_usable_requires_generation_check,
    DROP COLUMN IF EXISTS active_generation_usable;
