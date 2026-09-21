-- push token과 APNs 환경은 함께 있거나 함께 없다.
--
-- 00001의 devices_push_environment_check는 값의 범위만 검사한다. 주석은 "token이
-- 없으면 environment도 없다"고 적었지만 제약은 그것을 강제하지 않았다. token 없이
-- environment만 있으면 무해하지만, token이 있는데 environment가 없으면 worker가
-- sandbox와 production 중 어디로 보낼지 알 수 없고 APNs가 거부한다(설계 9).
--
-- PUT /v1/devices/{id}/push-token이 이 두 열을 처음 채우는 경로라, 그 endpoint와
-- 함께 DB가 강제하도록 올린다. 00001을 고치지 않고 새 마이그레이션으로 둔다.
-- 이미 적용된 마이그레이션을 고치면 적용된 DB와 파일이 갈라진다.

-- +goose Up
ALTER TABLE devices
    ADD CONSTRAINT devices_push_token_environment_pairing_check CHECK (
        (push_token_ciphertext IS NULL) = (push_environment IS NULL)
    );

-- +goose Down
ALTER TABLE devices
    DROP CONSTRAINT IF EXISTS devices_push_token_environment_pairing_check;
