-- 캘린더 동기화 서버 스키마. 근거는 docs/calendar_privacy_sync_design.md §3, §5.2, §7, §11
-- 그리고 .omc/plans/calendar-sync-server-implementation.md C1.
--
-- 이번 범위(시연 트랙): 연결, 읽기 source 선택, snapshot session·page·staging, 활성 generation의
-- Busy facts. 제목·장소 ciphertext(공개 수준 details), writer device, write command,
-- app_confirmed_event_id는 각각 공개 수준·캘린더 쓰기 단계에서 ALTER로 추가한다.
--
-- 이 표들은 모두 사용자 소유 개인 데이터라 users에 ON DELETE CASCADE다(§11 "계정 삭제 시 facts,
-- projection, connection을 삭제한다", 설계 2 §8 FK 삭제 정책).
--
-- 잠금 순서 (모든 트랜잭션이 같은 순서를 지킨다):
--   users -> parties -> party_memberships -> party_invites -> calendar_connections
-- Party 트랜잭션(T1·T3)은 자기 Party를 잠근 뒤 마지막에 connection을 FOR SHARE로 잡고 facts를
-- 읽는다. 사용자의 snapshot complete와 연결 상태 변경은 같은 순서를 따르기 위해 사용자가 속한 활성
-- Party 행을 id 순으로 FOR SHARE로 먼저 잡고(party.LockUserPartiesShared), 그 다음 connection을
-- FOR UPDATE로 잡고, 멤버십 행은 FOR KEY SHARE로만 잡는다. Party를 FOR SHARE로 잡는 덕에 같은
-- Party에 가입하는 T3(parties FOR UPDATE)와 직렬화돼 신규 멤버가 수신자에서 빠지지 않는다.

-- +goose Up

-- 사용자당 연결 하나. source_status는 설계 §5.2의 읽기 축이다. stale은 저장하지 않고
-- last_completed_sync_at으로 계산한다(마지막 정상 sync 6시간 초과).
CREATE TABLE calendar_connections (
    id                    uuid        PRIMARY KEY,
    user_id               uuid        NOT NULL,
    sync_device_id        uuid,
    source_status         text        NOT NULL DEFAULT 'disconnected',
    active_generation     bigint      NOT NULL DEFAULT 0,
    -- 마지막으로 받아들인 snapshot revision. session row를 24시간 뒤 지워도 revision 단조성을 유지한다.
    last_snapshot_revision bigint     NOT NULL DEFAULT 0,
    last_completed_sync_at timestamptz,
    time_zone             text,
    -- 권한 철회·거부가 시작된 시각. 7일 안에 재연결하지 않으면 facts를 지운다(§12).
    revoked_at            timestamptz,
    version               bigint      NOT NULL DEFAULT 1,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT calendar_connections_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    -- 기기가 폐기·삭제돼도 연결은 남는다. sync device를 다시 정해야 하는 상태가 된다.
    CONSTRAINT calendar_connections_sync_device_id_fkey
        FOREIGN KEY (sync_device_id) REFERENCES devices (id) ON DELETE SET NULL,
    CONSTRAINT calendar_connections_source_status_check CHECK (source_status IN (
        'disconnected', 'permission_required', 'selecting', 'syncing', 'ready',
        'denied', 'restricted', 'revoked', 'needs_source_reselection', 'error')),
    CONSTRAINT calendar_connections_version_positive_check CHECK (version > 0),
    CONSTRAINT calendar_connections_generation_check CHECK (active_generation >= 0),
    -- ready이면 정상 완료 sync가 반드시 있었다. 없는데 ready로 보이면 "일정 없음"으로 오인된다(§5.2).
    CONSTRAINT calendar_connections_ready_requires_sync_check CHECK (
        source_status <> 'ready' OR (last_completed_sync_at IS NOT NULL AND active_generation > 0)
    )
);
CREATE UNIQUE INDEX calendar_connections_user_key ON calendar_connections (user_id);

-- 읽기 대상 캘린더. source_key는 기기 로컬 키를 그대로 저장하지 않고 앱이 만든 불투명 문자열이다.
-- calendar 이름은 저장하지 않는다(§3.1, §11).
CREATE TABLE calendar_source_selections (
    connection_id uuid        NOT NULL,
    source_key    text        NOT NULL,
    enabled       boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, source_key),
    CONSTRAINT calendar_source_selections_connection_id_fkey
        FOREIGN KEY (connection_id) REFERENCES calendar_connections (id) ON DELETE CASCADE,
    CONSTRAINT calendar_source_selections_key_length_check CHECK (char_length(source_key) BETWEEN 1 AND 128)
);

-- snapshot session. revision은 연결 안에서 단조 증가하는 클라이언트 값이다. 열린 session은
-- 연결당 하나뿐이다(새 session이 이전 열린 session을 abort한다).
CREATE TABLE calendar_snapshot_sessions (
    id             uuid        PRIMARY KEY,
    connection_id  uuid        NOT NULL,
    revision       bigint      NOT NULL,
    window_start   timestamptz NOT NULL,
    window_end     timestamptz NOT NULL,
    expected_pages integer     NOT NULL,
    status         text        NOT NULL DEFAULT 'open',
    -- complete가 받아들인 fact 수와 부여한 generation. 같은 revision 재호출에 같은 결과를 준다.
    fact_count     integer,
    generation     bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz,
    CONSTRAINT calendar_snapshot_sessions_connection_id_fkey
        FOREIGN KEY (connection_id) REFERENCES calendar_connections (id) ON DELETE CASCADE,
    CONSTRAINT calendar_snapshot_sessions_status_check CHECK (status IN ('open', 'completed', 'aborted')),
    CONSTRAINT calendar_snapshot_sessions_window_check CHECK (window_end > window_start),
    CONSTRAINT calendar_snapshot_sessions_pages_check CHECK (expected_pages BETWEEN 0 AND 200),
    CONSTRAINT calendar_snapshot_sessions_revision_positive_check CHECK (revision > 0),
    CONSTRAINT calendar_snapshot_sessions_completed_check CHECK (
        (status = 'completed') = (generation IS NOT NULL AND completed_at IS NOT NULL AND fact_count IS NOT NULL)
    )
);
CREATE UNIQUE INDEX calendar_snapshot_sessions_revision_key
    ON calendar_snapshot_sessions (connection_id, revision);
CREATE UNIQUE INDEX calendar_snapshot_sessions_one_open_key
    ON calendar_snapshot_sessions (connection_id) WHERE status = 'open';
-- 24시간 뒤 정리(§11).
CREATE INDEX calendar_snapshot_sessions_cleanup_idx ON calendar_snapshot_sessions (created_at);

-- 업로드된 page 표시. 빈 page도 업로드했다는 사실이 필요하므로 facts와 별도로 둔다.
CREATE TABLE calendar_snapshot_pages (
    session_id uuid    NOT NULL,
    page       integer NOT NULL,
    fact_count integer NOT NULL,
    PRIMARY KEY (session_id, page),
    CONSTRAINT calendar_snapshot_pages_session_id_fkey
        FOREIGN KEY (session_id) REFERENCES calendar_snapshot_sessions (id) ON DELETE CASCADE,
    CONSTRAINT calendar_snapshot_pages_page_check CHECK (page >= 0),
    CONSTRAINT calendar_snapshot_pages_count_check CHECK (fact_count BETWEEN 0 AND 500)
);

-- 업로드 중인 facts. 검증이 끝나기 전에는 어느 계산에도 쓰이지 않는다.
CREATE TABLE calendar_snapshot_facts_staging (
    session_id       uuid        NOT NULL,
    page             integer     NOT NULL,
    source_event_key bytea       NOT NULL,
    start_at         timestamptz NOT NULL,
    end_at           timestamptz NOT NULL,
    all_day          boolean     NOT NULL,
    time_zone        text        NOT NULL,
    availability     text        NOT NULL,
    PRIMARY KEY (session_id, source_event_key),
    CONSTRAINT calendar_snapshot_facts_staging_session_id_fkey
        FOREIGN KEY (session_id) REFERENCES calendar_snapshot_sessions (id) ON DELETE CASCADE,
    CONSTRAINT calendar_snapshot_facts_staging_range_check CHECK (end_at > start_at),
    CONSTRAINT calendar_snapshot_facts_staging_availability_check CHECK (availability IN ('busy', 'tentative', 'unavailable')),
    CONSTRAINT calendar_snapshot_facts_staging_key_length_check CHECK (octet_length(source_event_key) BETWEEN 16 AND 32)
);
CREATE INDEX calendar_snapshot_facts_staging_page_idx ON calendar_snapshot_facts_staging (session_id, page);

-- 활성 generation의 Busy facts. 소유자 본인과 서버의 projection·availability service만 읽는다.
-- complete가 연결의 모든 row를 지우고 새 generation으로 다시 채운다(같은 트랜잭션).
CREATE TABLE calendar_busy_facts (
    connection_id    uuid        NOT NULL,
    source_event_key bytea       NOT NULL,
    user_id          uuid        NOT NULL,
    generation       bigint      NOT NULL,
    start_at         timestamptz NOT NULL,
    end_at           timestamptz NOT NULL,
    all_day          boolean     NOT NULL,
    time_zone        text        NOT NULL,
    availability     text        NOT NULL,
    PRIMARY KEY (connection_id, source_event_key),
    CONSTRAINT calendar_busy_facts_connection_id_fkey
        FOREIGN KEY (connection_id) REFERENCES calendar_connections (id) ON DELETE CASCADE,
    CONSTRAINT calendar_busy_facts_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT calendar_busy_facts_range_check CHECK (end_at > start_at),
    CONSTRAINT calendar_busy_facts_availability_check CHECK (availability IN ('busy', 'tentative', 'unavailable'))
);
-- Busy 범위 조회(§11).
CREATE INDEX calendar_busy_facts_user_range_idx ON calendar_busy_facts (user_id, start_at, end_at);

-- +goose Down
DROP TABLE IF EXISTS calendar_busy_facts;
DROP TABLE IF EXISTS calendar_snapshot_facts_staging;
DROP TABLE IF EXISTS calendar_snapshot_pages;
DROP TABLE IF EXISTS calendar_snapshot_sessions;
DROP TABLE IF EXISTS calendar_source_selections;
DROP TABLE IF EXISTS calendar_connections;
