-- Party 생성·초대·역할·탈퇴의 스키마. 근거는 docs/party_membership_design.md §5.1과
-- docs/party_visibility_design.md §6.
--
-- account_backend_design.md §8은 parties, party_memberships, party_invites,
-- party_visibility_settings, party_schedule_projections를 이미 고정했지만 P0
-- 골격(00001)은 인증·동기화 경로에 필요한 테이블만 만들었다. 그래서 이 마이그레이션은
-- 설계 4 §5.1이 말하는 "추가" 열만 붙이는 ALTER가 아니라 §8의 기존 열과 §5.1의 추가 열을
-- 합친 전체 CREATE다.
--
-- 00001의 규약을 그대로 따른다.
--   1. 상태 문자열은 enum이 아니라 이름 붙인 CHECK constraint다.
--   2. version bigint는 클라이언트가 expected_version으로 다루거나 sync가 투영하는 row에만 둔다.
--   3. 모든 ID는 uuid, 모든 시각은 timestamptz다.
--
-- version 열의 판정:
--   - parties                     : §5.5 "모든 Party mutation에서 expected_version은
--                                   parties.version을 의미한다" -> 있음
--   - party_memberships           : §5.1 "멤버십 row의 version은 sync 투영용" -> 있음
--   - party_visibility_settings   : 공개 수준 §6.1 "mutation마다 증가" -> 있음
--   - party_schedule_projections  : 공개 수준 §6.2 "sync 충돌 해결" -> 있음
--   - party_invites               : 클라이언트가 expected_version을 보내는 경로가 없고
--                                   sync는 초대를 방장에게 전체 투영한다 -> 없음, updated_at만 둔다

-- +goose Up

-- Party. 상태 전이는 §5.2: active -> disbanded (종단).
--
-- owner_membership_id는 NOT NULL이지만 FK는 지연된다. §5.1 "검증 시점": T1은 멤버십 row가
-- 생기기 전에 active Party를 insert하므로 커밋 전까지는 존재하지 않는 멤버십을 가리킨다.
-- NOT NULL은 PostgreSQL에서 지연 대상이 아니므로 T1이 멤버십 UUID를 미리 생성해야 한다.
--
-- 순환 FK(parties -> party_memberships -> parties)이므로 FK는 세 테이블을 만든 뒤 ALTER로 붙인다.
CREATE TABLE parties (
    id                  uuid        PRIMARY KEY,
    name                text        NOT NULL,
    status              text        NOT NULL DEFAULT 'active',
    version             bigint      NOT NULL DEFAULT 1,
    owner_membership_id uuid        NOT NULL,
    member_limit        smallint    NOT NULL DEFAULT 10,
    created_by_user_id  uuid        NOT NULL,
    disbanded_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- 협업 기록이므로 restrict 후 익명화다 (§8 FK 삭제 정책). created_by_user_id는
    -- 감사용이며 소유권과 무관하다(§5.1).
    CONSTRAINT parties_created_by_user_id_fkey
        FOREIGN KEY (created_by_user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT parties_status_check CHECK (status IN ('active', 'disbanded')),
    CONSTRAINT parties_version_positive_check CHECK (version > 0),
    CONSTRAINT parties_member_limit_check CHECK (member_limit BETWEEN 1 AND 10),
    -- users_deleted_at_requires_terminal_status_check와 같은 이유로 한쪽 방향만 막는다.
    -- active Party에 해산 시각이 찍히는 것을 막을 뿐, disbanded 전환 문장이 반드시 같은
    -- 문장에서 시각을 쓰도록 강제하지는 않는다.
    CONSTRAINT parties_disbanded_at_requires_disbanded_check CHECK (
        disbanded_at IS NULL OR status = 'disbanded'
    )
);

CREATE INDEX parties_created_by_user_id_idx ON parties (created_by_user_id);

-- 멤버십. 상태 전이는 §5.3. left와 removed는 종단이며 재가입은 기존 row를 되살리지 않고
-- 새 row를 만든다. 그래서 (party_id, user_id)는 활성 row에 대해서만 unique다.
CREATE TABLE party_memberships (
    id         uuid        PRIMARY KEY,
    party_id   uuid        NOT NULL,
    user_id    uuid        NOT NULL,
    role       text        NOT NULL DEFAULT 'member',
    status     text        NOT NULL DEFAULT 'active',
    version    bigint      NOT NULL DEFAULT 1,
    joined_at  timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz,
    end_reason text,
    invite_id  uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT party_memberships_party_id_fkey
        FOREIGN KEY (party_id) REFERENCES parties (id) ON DELETE RESTRICT,
    -- 협업 기록이므로 restrict 후 익명화다 (§8 FK 삭제 정책). 계정 삭제는 §5.8에 따라
    -- 멤버십을 end_reason='account_deleted'로 종료시키지 이 row를 물리 삭제하지 않는다.
    CONSTRAINT party_memberships_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT party_memberships_role_check CHECK (role IN ('owner', 'member')),
    CONSTRAINT party_memberships_status_check CHECK (status IN ('active', 'left', 'removed')),
    CONSTRAINT party_memberships_version_positive_check CHECK (version > 0),
    -- 종료 상태와 종료 기록은 함께 움직인다. 활성 멤버십에 종료 시각이 남거나 종료된
    -- 멤버십에 사유가 비는 것을 막는다.
    CONSTRAINT party_memberships_end_fields_match_status_check CHECK (
        (status = 'active' AND ended_at IS NULL AND end_reason IS NULL)
        OR (status <> 'active' AND ended_at IS NOT NULL AND end_reason IS NOT NULL)
    ),
    -- §5.3 전이도의 사유 분기를 그대로 강제한다. 본인 의사로 끝난 것은 left,
    -- 타인·시스템이 끝낸 것은 removed다.
    CONSTRAINT party_memberships_end_reason_check CHECK (
        end_reason IS NULL
        OR (status = 'left' AND end_reason IN ('left_voluntarily', 'account_deleted'))
        OR (status = 'removed' AND end_reason IN ('removed_by_owner', 'party_disbanded'))
    )
);

-- §8 필수 불변식: Party의 활성 멤버는 (party_id, user_id)당 하나다.
CREATE UNIQUE INDEX party_memberships_active_member_key
    ON party_memberships (party_id, user_id) WHERE status = 'active';

-- §5.1: 한 Party에 role='owner' AND status='active'인 멤버십은 최대 1개다.
--
-- unique index는 지연할 수 없으므로 이 검사는 문장마다 즉시 일어난다. 그래서 T4(위임)는
-- 기존 방장 강등과 대상 승격을 하나의 UPDATE ... CASE로 처리할 수 없고 강등을 먼저 실행한
-- 뒤 승격해야 한다. active Party에서 "정확히 1개"를 만드는 나머지 절반은 아래 지연
-- 트리거 party_assert_active_owner_membership이 담당한다.
CREATE UNIQUE INDEX party_memberships_active_owner_key
    ON party_memberships (party_id) WHERE role = 'owner' AND status = 'active';

-- 사용자별 참여 Party 조회와 §4.3 소유 Party 한도 검사 경로.
CREATE INDEX party_memberships_active_user_idx
    ON party_memberships (user_id) WHERE status = 'active';

-- 초대. 상태 전이는 §5.4.
--
-- token_hash가 NULL을 허용하는 이유는 §5.1이다. 만료·소진된 초대의 해시는 90일 뒤 정리
-- 작업이 NULL로 지우고 row는 감사 목적으로 남긴다. 그 대가로 전역 unique가 아니라 부분
-- unique를 쓴다.
CREATE TABLE party_invites (
    id                       uuid        PRIMARY KEY,
    party_id                 uuid        NOT NULL,
    token_hash               bytea,
    status                   text        NOT NULL DEFAULT 'active',
    expires_at               timestamptz NOT NULL,
    max_uses                 smallint    NOT NULL,
    used_count               smallint    NOT NULL DEFAULT 0,
    created_by_membership_id uuid        NOT NULL,
    revoked_at               timestamptz,
    last_used_at             timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT party_invites_party_id_fkey
        FOREIGN KEY (party_id) REFERENCES parties (id) ON DELETE RESTRICT,
    CONSTRAINT party_invites_created_by_membership_id_fkey
        FOREIGN KEY (created_by_membership_id) REFERENCES party_memberships (id) ON DELETE RESTRICT,
    CONSTRAINT party_invites_status_check CHECK (
        status IN ('active', 'revoked', 'expired', 'exhausted')
    ),
    CONSTRAINT party_invites_max_uses_positive_check CHECK (max_uses > 0),
    CONSTRAINT party_invites_used_count_non_negative_check CHECK (used_count >= 0),
    -- §5.1 추가 불변식. 정원 경쟁에서 used_count가 max_uses를 넘지 않는다.
    CONSTRAINT party_invites_used_count_within_max_check CHECK (used_count <= max_uses),
    CONSTRAINT party_invites_revoked_at_requires_revoked_check CHECK (
        revoked_at IS NULL OR status = 'revoked'
    )
);

-- §5.1: 살아 있는 token만 유일하다. 정리로 NULL이 된 감사 row는 서로 충돌하지 않는다.
CREATE UNIQUE INDEX party_invites_token_hash_key
    ON party_invites (token_hash) WHERE token_hash IS NOT NULL;

-- 방장의 초대 목록 조회와 T7의 활성 초대 일괄 무효화 경로.
CREATE INDEX party_invites_active_party_idx
    ON party_invites (party_id) WHERE status = 'active';

-- 순환 참조 FK. §5.1에 따라 지연한다. T1은 parties를 먼저 insert하고 멤버십을 뒤에 만든다.
ALTER TABLE parties
    ADD CONSTRAINT parties_owner_membership_id_fkey
    FOREIGN KEY (owner_membership_id) REFERENCES party_memberships (id)
    DEFERRABLE INITIALLY DEFERRED;

-- 가입 경로 추적용. 생성자 멤버십은 NULL이다(§5.1).
ALTER TABLE party_memberships
    ADD CONSTRAINT party_memberships_invite_id_fkey
    FOREIGN KEY (invite_id) REFERENCES party_invites (id) ON DELETE RESTRICT;

-- 본인의 Party별 공개 수준. 계약은 party_visibility_design.md §6.1.
--
-- 설계는 (party_id, user_id) 복합 unique만 요구하지만 id를 별도로 둔다. §9.2의 sync entity
-- type party_visibility_setting이 entity_id를 필요로 하고, 복합 키를 entity_id 한 열에
-- 담을 수 없기 때문이다.
--
-- "활성 멤버십마다 본인 소유 설정이 정확히 1건"(공개 수준 §5 불변식 1)은 여기서 강제하지
-- 않는다. 생성 책임이 T1·T3에 있고 검증 계약은 설계 5 구현 범위다.
CREATE TABLE party_visibility_settings (
    id               uuid        PRIMARY KEY,
    party_id         uuid        NOT NULL,
    user_id          uuid        NOT NULL,
    visibility_level text        NOT NULL DEFAULT 'busyOnly',
    share_location   boolean     NOT NULL DEFAULT false,
    version          bigint      NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT party_visibility_settings_party_id_fkey
        FOREIGN KEY (party_id) REFERENCES parties (id) ON DELETE RESTRICT,
    -- 본인 전용 개인 설정이므로 cascade다 (§8 FK 삭제 정책).
    CONSTRAINT party_visibility_settings_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT party_visibility_settings_level_check CHECK (
        visibility_level IN ('details', 'busyOnly', 'hidden')
    ),
    CONSTRAINT party_visibility_settings_version_positive_check CHECK (version > 0),
    -- 공개 수준 §6.1: details가 아니면 장소를 공유하지 않는다.
    CONSTRAINT party_visibility_settings_share_location_check CHECK (
        share_location = false OR visibility_level = 'details'
    )
);

CREATE UNIQUE INDEX party_visibility_settings_party_user_key
    ON party_visibility_settings (party_id, user_id);

-- Party 화면에 보이는 일정 투영. 계약은 party_visibility_design.md §6.2.
--
-- id는 Party 범위 무작위 UUID이며 API와 sync가 쓰는 유일한 일정 식별자다.
-- source_event_key는 내부 join 전용이고 API·sync·로그에 나가면 안 된다.
CREATE TABLE party_schedule_projections (
    id                  uuid        PRIMARY KEY,
    party_id            uuid        NOT NULL,
    owner_user_id       uuid        NOT NULL,
    source_event_key    bytea       NOT NULL,
    source_generation   bigint      NOT NULL,
    setting_version     bigint      NOT NULL,
    visibility_level    text        NOT NULL,
    start_at            timestamptz NOT NULL,
    end_at              timestamptz NOT NULL,
    all_day             boolean     NOT NULL DEFAULT false,
    time_zone           text        NOT NULL,
    title_ciphertext    bytea,
    location_ciphertext bytea,
    version             bigint      NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT party_schedule_projections_party_id_fkey
        FOREIGN KEY (party_id) REFERENCES parties (id) ON DELETE RESTRICT,
    -- 본인 일정에서 파생된 개인 데이터이므로 cascade다. §7.3은 관계가 끝나면 투영이
    -- 남지 않아야 한다고 요구한다.
    CONSTRAINT party_schedule_projections_owner_user_id_fkey
        FOREIGN KEY (owner_user_id) REFERENCES users (id) ON DELETE CASCADE,
    -- 공개 수준 §6.2: hidden은 투영 자체가 없으므로 저장할 수 없다.
    CONSTRAINT party_schedule_projections_level_check CHECK (
        visibility_level IN ('details', 'busyOnly')
    ),
    CONSTRAINT party_schedule_projections_version_positive_check CHECK (version > 0),
    CONSTRAINT party_schedule_projections_time_range_check CHECK (end_at > start_at),
    -- 공개 수준 §6.2: busyOnly row에 상세 ciphertext가 있으면 commit을 거부한다.
    --
    -- 같은 절의 "details인데 share_location=false면 location ciphertext 금지"는 여기서
    -- 막을 수 없다. share_location이 다른 테이블의 열이라 row CHECK의 가시 범위 밖이다.
    -- 설계가 "DB 또는 service validation"으로 허용한 대로 service가 담당한다.
    CONSTRAINT party_schedule_projections_busy_only_has_no_details_check CHECK (
        visibility_level <> 'busyOnly'
        OR (title_ciphertext IS NULL AND location_ciphertext IS NULL)
    )
);

-- 공개 수준 §6.2: unique key는 (party_id, owner_user_id, source_event_key)다.
CREATE UNIQUE INDEX party_schedule_projections_source_key
    ON party_schedule_projections (party_id, owner_user_id, source_event_key);

-- 범위 조회는 §8 "일정 범위 조회는 (user_id, start_at, end_at) index"를 Party 범위로 옮긴다.
CREATE INDEX party_schedule_projections_range_idx
    ON party_schedule_projections (party_id, start_at, end_at);

-- 지연 검증 트리거 3종.
--
-- §5.1 "검증 시점": 아래 세 불변식은 트랜잭션 중간에 일시적으로 깨진다. T1은 멤버십이
-- 생기기 전에 active Party를 insert하고, T7은 멤버십을 종료하기 전에 Party를 disbanded로
-- 바꾼다. 따라서 row 단위 CHECK가 아니라 커밋 시점에 검증하는 CONSTRAINT TRIGGER여야 한다.
--
-- 세 함수 모두 검사 대상 Party의 id를 트리거 인자로 받은 열 이름에서 꺼낸다. parties에는
-- id, 하위 테이블에는 party_id로 이름이 달라서 인자로 받는다.
--
-- 오류는 SQLSTATE 23514(check_violation)와 제약 이름으로 던진다. 테스트가 "그냥 오류가
-- 났다"가 아니라 "그 불변식이 거부했다"를 확인할 수 있어야 한다.

-- active Party의 owner_membership_id 3조건: 같은 Party, status='active', role='owner'.
-- disbanded Party의 owner_membership_id는 마지막 방장을 가리키는 감사 참조이며 대상이 아니다.
-- +goose StatementBegin
CREATE FUNCTION party_assert_active_owner_membership() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target uuid;
BEGIN
    -- DELETE에서 NEW는 할당되지 않으므로 분기한다. TG_OP/NEW/OLD/TG_ARGV는 트리거
    -- 함수의 지역 변수이므로 공용 헬퍼로 뽑아낼 수 없고 여기서 직접 읽는다.
    IF TG_OP = 'DELETE' THEN
        target := (to_jsonb(OLD) ->> TG_ARGV[0])::uuid;
    ELSE
        target := (to_jsonb(NEW) ->> TG_ARGV[0])::uuid;
    END IF;

    IF target IS NULL THEN
        RETURN NULL;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM parties WHERE id = target AND status = 'active') THEN
        RETURN NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM parties p
        JOIN party_memberships m ON m.id = p.owner_membership_id
        WHERE p.id = target
          AND m.party_id = p.id
          AND m.status = 'active'
          AND m.role = 'owner'
    ) THEN
        RAISE EXCEPTION
            'active Party %의 owner_membership_id가 같은 Party의 활성 방장 멤버십을 가리키지 않는다', target
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'party_active_owner_membership_check';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- active Party는 활성 멤버가 1명 이상이다. 마지막 활성 멤버가 사라지는 트랜잭션은 반드시
-- 같은 트랜잭션에서 Party를 disbanded로 만든다(§5.1, T5의 단독 방장 탈퇴 -> 해산 전환).
-- +goose StatementBegin
CREATE FUNCTION party_assert_active_party_has_member() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target uuid;
BEGIN
    -- DELETE에서 NEW는 할당되지 않으므로 분기한다. TG_OP/NEW/OLD/TG_ARGV는 트리거
    -- 함수의 지역 변수이므로 공용 헬퍼로 뽑아낼 수 없고 여기서 직접 읽는다.
    IF TG_OP = 'DELETE' THEN
        target := (to_jsonb(OLD) ->> TG_ARGV[0])::uuid;
    ELSE
        target := (to_jsonb(NEW) ->> TG_ARGV[0])::uuid;
    END IF;

    IF target IS NULL THEN
        RETURN NULL;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM parties WHERE id = target AND status = 'active') THEN
        RETURN NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM party_memberships
        WHERE party_id = target AND status = 'active'
    ) THEN
        RAISE EXCEPTION 'active Party %에 활성 멤버가 없다', target
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'party_active_party_has_member_check';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- disbanded Party에는 활성 멤버십, 활성 초대, 활성 projection이 없다(§5.1).
-- projection에는 상태 열이 없으므로 존재 자체가 활성이다. T5·T7이 삭제한다.
-- +goose StatementBegin
CREATE FUNCTION party_assert_disbanded_has_no_active_children() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target uuid;
BEGIN
    -- DELETE에서 NEW는 할당되지 않으므로 분기한다. TG_OP/NEW/OLD/TG_ARGV는 트리거
    -- 함수의 지역 변수이므로 공용 헬퍼로 뽑아낼 수 없고 여기서 직접 읽는다.
    IF TG_OP = 'DELETE' THEN
        target := (to_jsonb(OLD) ->> TG_ARGV[0])::uuid;
    ELSE
        target := (to_jsonb(NEW) ->> TG_ARGV[0])::uuid;
    END IF;

    IF target IS NULL THEN
        RETURN NULL;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM parties WHERE id = target AND status = 'disbanded') THEN
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1 FROM party_memberships WHERE party_id = target AND status = 'active'
    ) THEN
        RAISE EXCEPTION 'disbanded Party %에 활성 멤버십이 남아 있다', target
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'party_disbanded_has_no_active_children_check';
    END IF;

    IF EXISTS (
        SELECT 1 FROM party_invites WHERE party_id = target AND status = 'active'
    ) THEN
        RAISE EXCEPTION 'disbanded Party %에 활성 초대가 남아 있다', target
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'party_disbanded_has_no_active_children_check';
    END IF;

    IF EXISTS (
        SELECT 1 FROM party_schedule_projections WHERE party_id = target
    ) THEN
        RAISE EXCEPTION 'disbanded Party %에 projection이 남아 있다', target
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'party_disbanded_has_no_active_children_check';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- 트리거 부착. 불변식을 깨뜨릴 수 있는 모든 경로에 건다.
--
-- Party row가 사라지는 DELETE에는 걸지 않는다. 세 검사 모두 "그 Party가 존재할 때"만
-- 성립하며, parties DELETE는 하위 FK가 RESTRICT라 하위 row가 남아 있으면 먼저 막힌다.

CREATE CONSTRAINT TRIGGER parties_assert_active_owner_membership
    AFTER INSERT OR UPDATE ON parties
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_active_owner_membership('id');

CREATE CONSTRAINT TRIGGER party_memberships_assert_active_owner_membership
    AFTER INSERT OR UPDATE OR DELETE ON party_memberships
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_active_owner_membership('party_id');

CREATE CONSTRAINT TRIGGER parties_assert_active_party_has_member
    AFTER INSERT OR UPDATE ON parties
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_active_party_has_member('id');

CREATE CONSTRAINT TRIGGER party_memberships_assert_active_party_has_member
    AFTER INSERT OR UPDATE OR DELETE ON party_memberships
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_active_party_has_member('party_id');

CREATE CONSTRAINT TRIGGER parties_assert_disbanded_is_empty
    AFTER INSERT OR UPDATE ON parties
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_disbanded_has_no_active_children('id');

CREATE CONSTRAINT TRIGGER party_memberships_assert_disbanded_is_empty
    AFTER INSERT OR UPDATE ON party_memberships
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_disbanded_has_no_active_children('party_id');

CREATE CONSTRAINT TRIGGER party_invites_assert_disbanded_is_empty
    AFTER INSERT OR UPDATE ON party_invites
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_disbanded_has_no_active_children('party_id');

CREATE CONSTRAINT TRIGGER party_schedule_projections_assert_disbanded_is_empty
    AFTER INSERT OR UPDATE ON party_schedule_projections
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION party_assert_disbanded_has_no_active_children('party_id');

-- +goose Down

DROP TABLE IF EXISTS party_schedule_projections;
DROP TABLE IF EXISTS party_visibility_settings;
ALTER TABLE IF EXISTS party_memberships DROP CONSTRAINT IF EXISTS party_memberships_invite_id_fkey;
ALTER TABLE IF EXISTS parties DROP CONSTRAINT IF EXISTS parties_owner_membership_id_fkey;
DROP TABLE IF EXISTS party_invites;
DROP TABLE IF EXISTS party_memberships;
DROP TABLE IF EXISTS parties;
DROP FUNCTION IF EXISTS party_assert_disbanded_has_no_active_children();
DROP FUNCTION IF EXISTS party_assert_active_party_has_member();
DROP FUNCTION IF EXISTS party_assert_active_owner_membership();
