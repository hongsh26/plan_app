-- P0 서버 골격의 기준 스키마. 근거는 docs/account_backend_design.md §8.
--
-- 이 마이그레이션이 P1~P8 전체에 물려주는 규약 세 가지:
--
-- 1. 상태 문자열은 PostgreSQL enum이 아니라 이름 붙인 CHECK constraint로 둔다.
--    후속 설계가 계속 허용 값을 추가하므로 ALTER TYPE 마찰을 피한다. 값을 늘릴 때는
--    DROP CONSTRAINT <이름> 후 ADD CONSTRAINT <이름>으로 교체한다. 이름을 생략하면
--    생성 이름에 의존하게 되어 이 교체가 불가능해지므로 모든 CHECK에 이름을 붙인다.
--
-- 2. version bigint는 클라이언트가 /v1 endpoint로 직접 수정하는 row에만 둔다.
--    §6이 "수정/삭제는 If-Match 또는 expected_version을 요구한다"고 했으므로
--    낙관적 동시성 토큰이 필요한 대상은 §6 endpoint 표에 mutation이 있는 테이블이다.
--      - users   : PATCH /v1/me, DELETE /v1/me          -> version 있음
--      - devices : DELETE /v1/devices/{id}, PUT .../push-token -> version 있음
--      - sessions, auth_identities, idempotency_keys, outbox_jobs
--                : 서버 내부 상태이고 클라이언트가 expected_version을 보내는 경로가
--                  없다 -> updated_at만 둔다.
--    판정을 가르는 것은 §6 endpoint 표이지 §8 필드 목록이 아니다. §8 표에는
--    devices 행에도 version이 없지만 devices는 §6에 클라이언트 mutation이 있다.
--      - sync_changes, audit_events : append-only -> created_at만 둔다.
--
-- 3. 모든 ID는 UUID, 모든 시각은 timestamptz다.

-- +goose Up
-- +goose StatementBegin

-- 사용자. 상태 전이는 §5.3:
--   active -> deletion_requested -> deleting -> deleted
--      +---> disabled --------------------------> deleted
CREATE TABLE users (
    id            uuid        PRIMARY KEY,
    display_name  text        NOT NULL,
    status        text        NOT NULL DEFAULT 'active',
    version       bigint      NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    CONSTRAINT users_status_check CHECK (
        status IN ('active', 'disabled', 'deletion_requested', 'deleting', 'deleted')
    ),
    CONSTRAINT users_version_positive_check CHECK (version > 0),
    -- deleted_at은 삭제 진행 중이거나 삭제된 계정에만 있다.
    --
    -- 한쪽 방향만 강제한다. "deleted면 반드시 deleted_at이 있다"까지 걸면
    -- §10의 삭제 파이프라인이 status를 deleted로 바꾸는 바로 그 문장에서
    -- deleted_at을 함께 써야 하고, 실제 삭제 작업이 도는 deleting 단계에는
    -- 시각을 기록할 수 없게 된다. 여기서 막아야 할 것은 active 계정에
    -- deleted_at이 찍히는 경우다.
    CONSTRAINT users_deleted_at_requires_terminal_status_check CHECK (
        deleted_at IS NULL OR status IN ('deleting', 'deleted')
    )
);

-- 외부 identity 연결. §5.1에 따라 Apple subject가 외부 계정 식별자이고
-- 이메일은 계정 키로 쓰지 않는다. 테이블은 다중 provider를 허용하되
-- Launch MVP API는 apple만 노출한다.
--
-- provider_refresh_token_ciphertext는 §11에 따라 KMS envelope encryption 결과만
-- 담는다. 평문 token을 넣지 않는다. 일반 api DB 역할은 복호화 권한을 갖지 않는다.
CREATE TABLE auth_identities (
    id                                uuid        PRIMARY KEY,
    user_id                           uuid        NOT NULL,
    provider                          text        NOT NULL,
    provider_subject                  text        NOT NULL,
    provider_refresh_token_ciphertext bytea,
    created_at                        timestamptz NOT NULL DEFAULT now(),
    updated_at                        timestamptz NOT NULL DEFAULT now(),
    -- 개인 자격 증명이므로 cascade다 (§8 FK 삭제 정책).
    CONSTRAINT auth_identities_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT auth_identities_provider_check CHECK (provider IN ('apple'))
);

-- §8 필수 불변식: 외부 identity 하나는 "활성" 사용자 하나에만 연결된다.
--
-- 조건 없는 unique인데도 "활성" 한정을 만족하는 이유는 §10 7단계가 deleted 전환과
-- 같은 트랜잭션에서 이 테이블의 row를 물리 삭제하기 때문이다. 삭제된 사용자는
-- identity를 보유하지 않으므로 살아 있는 row는 정의상 활성 사용자의 것뿐이다.
--
-- 이 index의 정확성은 그 삭제에 의존한다. 삭제 파이프라인에서 물리 삭제를 빼면
-- 같은 Apple 계정의 재가입이 영구히 막히고 §10 7단계의 "재로그인을 새 계정 생성
-- 흐름으로 처리한다"가 성립하지 않는다.
--
-- partial unique(detached_at 같은 열 + 조건부 index)로 바꾸지 말 것. 삭제된 계정에
-- 대해 Apple의 안정적 subject를 계속 보존하게 되어 §4 데이터 최소화와 충돌한다.
CREATE UNIQUE INDEX auth_identities_provider_subject_key
    ON auth_identities (provider, provider_subject);

CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);

-- 로그인·푸시 기기. device ID는 §5.2에 따라 서버가 발급하며 클라이언트가
-- 임의 값을 권한 근거로 만들 수 없다.
--
-- push_authorization / push_environment / time_sensitive_setting 3개 열은
-- 설계 9(docs/notification_design.md §17.2)가 요구한 것이다. 알림 구현 시점에
-- 두 번째 마이그레이션을 만들지 않으려고 지금 함께 만든다.
CREATE TABLE devices (
    id                     uuid        PRIMARY KEY,
    user_id                uuid        NOT NULL,
    platform               text        NOT NULL,
    push_token_ciphertext  bytea,
    push_authorization     text        NOT NULL DEFAULT 'unknown',
    push_environment       text,
    time_sensitive_setting text        NOT NULL DEFAULT 'not_supported',
    last_seen_at           timestamptz,
    revoked_at             timestamptz,
    version                bigint      NOT NULL DEFAULT 1,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    -- 기기는 cascade다 (§8 FK 삭제 정책).
    CONSTRAINT devices_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT devices_platform_check CHECK (platform IN ('ios')),
    CONSTRAINT devices_version_positive_check CHECK (version > 0),
    CONSTRAINT devices_push_authorization_check CHECK (
        push_authorization IN ('unknown', 'authorized', 'provisional', 'denied')
    ),
    -- push token이 없으면 environment도 없다. sandbox/production을 섞어 보내면
    -- APNs가 거부하므로 token과 함께 확정된다.
    CONSTRAINT devices_push_environment_check CHECK (
        push_environment IS NULL OR push_environment IN ('sandbox', 'production')
    ),
    CONSTRAINT devices_time_sensitive_setting_check CHECK (
        time_sensitive_setting IN ('enabled', 'disabled', 'not_supported')
    )
);

CREATE INDEX devices_user_id_idx ON devices (user_id);

-- 세션. §5.2의 refresh token 회전과 재사용 탐지를 담는다.
--   - refresh token 원문은 저장하지 않고 해시만 저장한다 (§11).
--   - token_family_id는 회전 사슬 하나를 묶는다. 이미 used_at이 찍힌 token이
--     다시 제시되면 같은 token_family_id의 모든 row를 폐기한다.
--   - used_at은 "이 refresh token이 이미 한 번 교환됐다"는 표시다.
CREATE TABLE sessions (
    id                 uuid        PRIMARY KEY,
    user_id            uuid        NOT NULL,
    device_id          uuid        NOT NULL,
    token_family_id    uuid        NOT NULL,
    refresh_token_hash bytea       NOT NULL,
    expires_at         timestamptz NOT NULL,
    used_at            timestamptz,
    revoked_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- 세션은 cascade다 (§8 FK 삭제 정책).
    CONSTRAINT sessions_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT sessions_device_id_fkey
        FOREIGN KEY (device_id) REFERENCES devices (id) ON DELETE CASCADE
);

-- 제시된 refresh token 해시로 세션을 한 번에 찾는다. 해시가 중복되면
-- 어느 세션인지 결정할 수 없으므로 unique다.
CREATE UNIQUE INDEX sessions_refresh_token_hash_key ON sessions (refresh_token_hash);

-- 재사용 탐지 시 token family 전체를 폐기하는 경로.
CREATE INDEX sessions_token_family_id_idx ON sessions (token_family_id);

-- 사용자별 활성 세션 조회와 계정 삭제 시 일괄 폐기 경로.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_device_id_idx ON sessions (device_id);

-- 서버 변경 피드. 개정된 §7.1을 따른다.
--
-- 순서 키는 (txid, ordinal)이다. 전역 BIGSERIAL 순번을 두지 않는다.
-- BIGSERIAL의 nextval은 트랜잭션 밖에서 즉시 소비되므로 번호 순서와 커밋 순서가
-- 일치하지 않는다. 먼저 번호를 받고 나중에 커밋한 트랜잭션의 변경이 cursor 뒤로
-- 밀려 영구 유실된다. 탈락시킨 대안과 근거는
-- docs/rec/2026-09-18_1719_sync_cursor_ordering_amendment.md에 있다.
--
-- 읽기는 settled horizon 아래만 반환한다:
--   horizon = pg_snapshot_xmin(pg_current_snapshot())
--   WHERE recipient_user_id = $1 AND (txid, ordinal) > cursor AND txid < horizon
--   ORDER BY txid, ordinal
--
-- ordinal은 트랜잭션 내부 counter이며 mutation transaction helper가 단독
-- 발급한다(P5에서 구현). 개별 call site가 직접 세지 않는다.
--
-- append-only이므로 version이 없다. 보존 30일 판정은 txid가 아니라 created_at으로
-- 한다 (§7.1).
CREATE TABLE sync_changes (
    txid              xid8        NOT NULL DEFAULT pg_current_xact_id(),
    ordinal           int         NOT NULL,
    recipient_user_id uuid        NOT NULL,
    entity_type       text        NOT NULL,
    entity_id         uuid        NOT NULL,
    operation         text        NOT NULL,
    entity_version    bigint,
    payload           jsonb       NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (txid, ordinal),
    -- 피드는 사용자별 파생 데이터이므로 cascade다.
    CONSTRAINT sync_changes_recipient_user_id_fkey
        FOREIGN KEY (recipient_user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT sync_changes_ordinal_non_negative_check CHECK (ordinal >= 0),
    -- 삭제와 탈퇴는 tombstone으로 전달한다 (§7.1).
    CONSTRAINT sync_changes_operation_check CHECK (
        operation IN ('upsert', 'tombstone')
    ),
    -- tombstone은 entity_version을 갖지 않는다.
    CONSTRAINT sync_changes_entity_version_check CHECK (
        (operation = 'upsert') = (entity_version IS NOT NULL)
    )
);

-- §8 필수 불변식: 사용자 cursor 조회를 보장하는 index.
CREATE INDEX sync_changes_recipient_cursor_idx
    ON sync_changes (recipient_user_id, txid, ordinal);

-- 30일 보존 하한 판정과 정리 작업 경로 (§7.1).
CREATE INDEX sync_changes_created_at_idx ON sync_changes (created_at);

-- 멱등성 키. §8 "멱등성 결과 보존": 전체 응답 본문이 아니라
-- status_code, resource_type, resource_id, resource_version만 저장한다.
-- 캘린더 상세와 token은 넣지 않는다.
CREATE TABLE idempotency_keys (
    id               uuid        PRIMARY KEY,
    user_id          uuid        NOT NULL,
    device_id        uuid        NOT NULL,
    key              text        NOT NULL,
    request_hash     bytea       NOT NULL,
    status_code      int,
    resource_type    text,
    resource_id      uuid,
    resource_version bigint,
    expires_at       timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT idempotency_keys_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT idempotency_keys_device_id_fkey
        FOREIGN KEY (device_id) REFERENCES devices (id) ON DELETE CASCADE
);

-- §8 필수 불변식: 같은 기기가 같은 키를 두 번 쓰면 원래 결과를 재사용한다.
CREATE UNIQUE INDEX idempotency_keys_user_device_key_key
    ON idempotency_keys (user_id, device_id, key);

-- 30일 만료 정리 경로.
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- 비동기 작업 outbox. §9의 job 상태:
--   pending -> running -> succeeded
--                 +----> retryable_failed -> pending
--                 +----> dead
--
-- 도메인 상태 전이와 같은 트랜잭션에서 커밋한다 (§8 필수 불변식).
-- worker는 FOR UPDATE SKIP LOCKED와 locked_until lease를 사용한다.
CREATE TABLE outbox_jobs (
    id            uuid        PRIMARY KEY,
    type          text        NOT NULL,
    payload       jsonb       NOT NULL,
    status        text        NOT NULL DEFAULT 'pending',
    attempt_count int         NOT NULL DEFAULT 0,
    next_run_at   timestamptz NOT NULL DEFAULT now(),
    locked_until  timestamptz,
    dedupe_key    text,
    last_error    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_jobs_status_check CHECK (
        status IN ('pending', 'running', 'succeeded', 'retryable_failed', 'dead')
    ),
    CONSTRAINT outbox_jobs_attempt_count_non_negative_check CHECK (attempt_count >= 0)
);

-- §8 필수 불변식: 동일 외부 작업은 활성 상태에서 하나만 존재한다.
-- succeeded와 dead는 활성이 아니므로 같은 dedupe_key를 다시 삽입할 수 있다.
CREATE UNIQUE INDEX outbox_jobs_active_dedupe_key
    ON outbox_jobs (type, dedupe_key)
    WHERE status IN ('pending', 'running', 'retryable_failed');

-- worker의 claim 쿼리 경로.
--
-- running을 포함해야 한다. §9는 "worker crash 후 locked_until이 지나면 다른
-- worker가 안전하게 재개한다"고 요구하는데, crash한 worker가 남기는 row는
-- status='running' + 만료된 locked_until이기 때문이다. running을 빼면 claim
-- 쿼리가 고아 row를 영원히 집지 않고, job이 dead로도 가지 못해 §14의 dead job
-- 경보에도 걸리지 않은 채 조용히 멈춘다.
--
-- claim 쿼리는 status로 후보를 좁힌 뒤 running에 대해서만 locked_until 만료를
-- 함께 확인한다.
CREATE INDEX outbox_jobs_claim_idx
    ON outbox_jobs (next_run_at)
    WHERE status IN ('pending', 'retryable_failed', 'running');

-- 보안 audit. §11에 따라 캘린더 내용과 token을 저장하지 않는다.
--
-- actor_user_id에 users FK를 걸지 않는다. append-only이므로 version도 없다.
--
-- §8의 FK 삭제 정책("협업 기록은 restrict 후 익명화")은 §10 5단계가 다루는
-- 멤버십·제안·확정 기록을 가리킨다. 보안 audit는 §10 6단계의 별개 절차이므로
-- 이 테이블은 그 정책의 대상이 아니다.
--
-- 다만 FK가 없으므로 §10 6단계의 tombstone 치환을 DB가 강제하지 않는다.
-- users row가 하드 삭제되면 audit row는 원본 UUID를 그대로 들고 남는다.
-- 치환은 삭제 worker의 책임이며, 보존 90일이 지나면 row 자체가 삭제된다.
-- (이전 주석은 "FK가 있으면 치환이 제약 위반이 된다"고 적었으나 사실이 아니다.
--  별도 actor_tombstone 열을 둔 이상 치환은 actor_user_id를 NULL로 바꾸는
--  것이어서 FK가 있어도 위반이 아니다. FK를 생략한 실제 이유는 위와 같다.)
CREATE TABLE audit_events (
    id            uuid        PRIMARY KEY,
    actor_user_id uuid,
    actor_tombstone text,
    action        text        NOT NULL,
    target_type   text,
    target_id     uuid,
    result        text        NOT NULL,
    request_id    uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_events_result_check CHECK (result IN ('success', 'failure')),
    -- 익명화 전에는 actor_user_id가, 익명화 후에는 actor_tombstone이 있다.
    -- 둘이 동시에 있으면 익명화가 끝나지 않은 것이므로 막는다.
    --
    -- 둘 다 NULL은 허용한다. §11이 별도 rate limit을 요구하는 인증과 초대 코드
    -- 검증은 정의상 user_id가 아직 없는 시점의 이벤트다. Apple credential 검증
    -- 실패와 인증 rate limit 거부가 대표적이고, 가장 감사하고 싶은 이벤트다.
    -- 행위자를 필수로 만들면 이것들이 스키마에 들어가지 못해 가짜 actor를
    -- 만들어 넣게 된다.
    CONSTRAINT audit_events_actor_check CHECK (
        NOT (actor_user_id IS NOT NULL AND actor_tombstone IS NOT NULL)
    )
);

CREATE INDEX audit_events_actor_user_id_idx
    ON audit_events (actor_user_id, created_at)
    WHERE actor_user_id IS NOT NULL;

-- 90일 뒤 삭제 경로 (§10 6단계).
CREATE INDEX audit_events_created_at_idx ON audit_events (created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS outbox_jobs;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS sync_changes;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS auth_identities;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
