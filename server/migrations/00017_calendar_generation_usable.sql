-- active generation을 현재 연결 상태에서 가능 시간 계산에 쓸 수 있는지 표시한다
-- (docs/availability_search_design.md §5.1, docs/rec/2026-10-28_1431_availability_search_api.md 결정 6).
--
-- source_status만으로는 철회 전 generation과 정상 재동기화 중인 generation을 구분할 수 없다.
-- ready → revoked → selecting → (snapshot 생성) syncing 경로에서 철회 전 facts가 다시 "재동기화 중"으로 보인다.
--   - snapshot complete로 새 generation이 활성화되면 true
--   - PUT으로 ready·syncing 밖의 상태가 되거나 읽기 source 선택이 바뀌면 false
--   - ready → syncing(정상 재동기화, 기기 전환)은 유지
-- projection·calendar.FreshUsers는 이 열을 보지 않는다(의미를 바꾸지 않는다).

-- +goose Up
ALTER TABLE calendar_connections
    ADD COLUMN active_generation_usable boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT calendar_connections_usable_requires_generation_check CHECK (
        NOT active_generation_usable OR active_generation > 0
    );
-- 기존 row: ready인 연결만 쓸 수 있다고 본다. syncing 등은 다음 complete까지 false(fail closed).
UPDATE calendar_connections SET active_generation_usable = true
 WHERE source_status = 'ready' AND active_generation > 0;

-- +goose Down
ALTER TABLE calendar_connections
    DROP CONSTRAINT IF EXISTS calendar_connections_usable_requires_generation_check,
    DROP COLUMN IF EXISTS active_generation_usable;
