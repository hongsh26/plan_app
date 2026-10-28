-- Snapshot 상세 allowlist staging. title/location은 calendar_busy_facts에 저장하지 않고,
-- snapshot complete transaction 안에서 party_schedule_projections로만 승격한 뒤 staging을 지운다.

-- +goose Up
ALTER TABLE calendar_snapshot_facts_staging
    ADD COLUMN detail_allowed boolean NOT NULL DEFAULT false,
    ADD COLUMN title_ciphertext bytea,
    ADD COLUMN location_ciphertext bytea,
    ADD CONSTRAINT calendar_snapshot_facts_staging_details_check CHECK (
        detail_allowed OR (title_ciphertext IS NULL AND location_ciphertext IS NULL)
    );

-- +goose Down
ALTER TABLE calendar_snapshot_facts_staging
    DROP CONSTRAINT IF EXISTS calendar_snapshot_facts_staging_details_check,
    DROP COLUMN IF EXISTS location_ciphertext,
    DROP COLUMN IF EXISTS title_ciphertext,
    DROP COLUMN IF EXISTS detail_allowed;
