-- Party projection 삭제는 캐시의 더 낮은 version 상세 upsert를 이겨야 한다.
-- 기존 tombstone은 version이 없어도 계속 허용한다.

-- +goose Up
ALTER TABLE sync_changes DROP CONSTRAINT sync_changes_entity_version_check;
ALTER TABLE sync_changes ADD CONSTRAINT sync_changes_entity_version_check CHECK (
    (operation = 'upsert' AND entity_version IS NOT NULL AND entity_version > 0)
    OR (operation = 'tombstone' AND (entity_version IS NULL OR entity_version > 0))
);

-- +goose Down
-- version이 있는 Party tombstone을 이미 보냈다면 되돌릴 수 없다. 로컬 reset 경로에서만 사용한다.
UPDATE sync_changes SET entity_version = NULL WHERE operation = 'tombstone';
ALTER TABLE sync_changes DROP CONSTRAINT sync_changes_entity_version_check;
ALTER TABLE sync_changes ADD CONSTRAINT sync_changes_entity_version_check CHECK (
    (operation = 'upsert') = (entity_version IS NOT NULL)
);
