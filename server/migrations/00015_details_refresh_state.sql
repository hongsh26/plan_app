-- 상세 snapshot 완료 여부를 설정 version에 묶어 상향 변경의 pending 상태를 해제한다.

-- +goose Up
ALTER TABLE calendar_snapshot_sessions
    ADD COLUMN details_included boolean NOT NULL DEFAULT false;
ALTER TABLE party_visibility_settings
    ADD COLUMN details_ready_version bigint,
    ADD CONSTRAINT party_visibility_settings_details_ready_version_check CHECK (
        details_ready_version IS NULL OR details_ready_version > 0
    );

-- +goose Down
ALTER TABLE party_visibility_settings
    DROP CONSTRAINT IF EXISTS party_visibility_settings_details_ready_version_check,
    DROP COLUMN IF EXISTS details_ready_version;
ALTER TABLE calendar_snapshot_sessions
    DROP COLUMN IF EXISTS details_included;
