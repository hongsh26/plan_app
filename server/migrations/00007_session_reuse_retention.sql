-- §5.2의 refresh token 재사용 탐지 보존 상한을 찾는 경로다. 기존 migration을
-- 수정하지 않는다. 이미 적용된 migration의 checksum과 실제 스키마가 갈라지기 때문이다.

-- +goose Up
CREATE INDEX sessions_used_at_idx ON sessions (used_at) WHERE used_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS sessions_used_at_idx;
