-- 사용자별 알림 참조 키. docs/notification_design.md §4.3·§17.2.
--
-- 알림 payload는 Party·entity UUID를 직접 싣지 않고 이 키로 파생한 HMAC 참조를
-- 싣는다. 기기는 같은 키로 로컬 후보를 대조해 역조회한다. 키는 인증된
-- /v1/sync/bootstrap과 GET /v1/me 응답으로만 전달한다.
--
-- 알림 구현보다 먼저 만드는 이유: 설계 9가 설계 2의 bootstrap·/v1/me 응답에 이
-- 필드를 추가하도록 개정했다. 응답 계약을 먼저 채워 두면 알림 구현 때 이미 배포된
-- 응답을 바꾸지 않아도 된다(00001에 devices 알림 열을 미리 넣은 것과 같은 이유).
--
-- 키는 암호화해서만 저장한다. api가 응답에 넣으려면 복호화해야 하므로, Apple
-- refresh token과 달리 api 역할에 복호화 권한이 필요한 값이다(KMS 구현 때 반영).
--
-- previous_ref_key_ciphertext: 회전 뒤에도 이미 만들어진 deferred 알림 job은 구 키로
-- 파생한 ref를 유지한다(설계 9 §4.3). 직전 키 하나를 90일 보관한다. 회전은 아직
-- 구현하지 않았다.

-- +goose Up
CREATE TABLE notification_ref_keys (
    user_id                     uuid        PRIMARY KEY,
    ref_key_ciphertext          bytea       NOT NULL,
    previous_ref_key_ciphertext bytea,
    rotated_at                  timestamptz NOT NULL DEFAULT now(),
    created_at                  timestamptz NOT NULL DEFAULT now(),
    -- 개인 키이므로 cascade다.
    CONSTRAINT notification_ref_keys_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS notification_ref_keys;
