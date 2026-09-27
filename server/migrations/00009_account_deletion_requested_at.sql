-- §10의 24시간 완료 목표를 계정 생성 시각이나 mutable updated_at이 아니라 삭제
-- 요청 시각에서 측정한다.

-- +goose Up
ALTER TABLE users ADD COLUMN deletion_requested_at timestamptz;
ALTER TABLE users ADD CONSTRAINT users_deletion_requested_at_status_check CHECK (
    deletion_requested_at IS NULL OR status IN ('deletion_requested', 'deleting', 'deleted')
);

-- +goose Down
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_deletion_requested_at_status_check;
ALTER TABLE users DROP COLUMN IF EXISTS deletion_requested_at;
