-- 사용자별 활동 시간 스키마. 근거는 docs/availability_search_design.md §3, §11 그리고
-- .omc/plans/availability-search-implementation.md A1.
--
-- 사용자 소유 개인 데이터라 users에 ON DELETE CASCADE다(설계 2 §8 FK 삭제 정책).
--
-- 설계 §11과 다른 점: 같은 사용자·요일의 overlap을 btree_gist exclusion constraint로 막지 않는다.
-- migration 역할에는 database CREATE 권한이 없어 CREATE EXTENSION을 할 수 없다
-- (docker/postgres-init/01-roles.sql). 대신 service가 저장 전에 정규화·병합하고, 아래 지연
-- constraint trigger가 커밋 시점에 같은 요일의 겹침과 맞닿음을 함께 거부한다. 저장된 interval은
-- 언제나 "병합까지 끝난 정규형"이라는 것이 DB 불변식이다. 계산 엔진은 이것에 기댈 수 있다.
--
-- trigger는 검사 전에 소유자의 availability_preferences row를 FOR NO KEY UPDATE로 잡는다. 같은 사용자의
-- 쓰기가 service 밖에서 동시에 일어나도 커밋 검사가 직렬화되고, 잠금 뒤의 검사 문장은 앞 트랜잭션이 커밋한
-- row를 본다. PUT은 트랜잭션 앞에서 같은 row를 먼저 잡으므로 추가 대기가 없다.

-- +goose Up

-- 사용자당 설정 하나. row가 없으면 unconfigured(version 0)로 본다. 'unconfigured' 값은 설정 초기화를
-- 위해 남겨 두지만 지금 API는 row를 configured로만 만든다.
CREATE TABLE availability_preferences (
    user_id    uuid        PRIMARY KEY,
    status     text        NOT NULL,
    time_zone  text,
    version    bigint      NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT availability_preferences_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT availability_preferences_status_check CHECK (status IN ('unconfigured', 'configured')),
    CONSTRAINT availability_preferences_time_zone_check CHECK (
        status <> 'configured' OR (time_zone IS NOT NULL AND time_zone <> '')
    ),
    CONSTRAINT availability_preferences_version_positive_check CHECK (version > 0)
);

-- 요일별 활동 구간. weekday는 ISO(1=월 ~ 7=일), 분은 사용자 time_zone의 지역 시각이다.
-- 자정을 넘는 입력은 service가 두 요일로 나눠 저장하므로 end_minute는 1440을 넘지 않는다.
CREATE TABLE availability_weekly_intervals (
    user_id      uuid     NOT NULL,
    weekday      smallint NOT NULL,
    position     smallint NOT NULL,
    start_minute integer  NOT NULL,
    end_minute   integer  NOT NULL,
    PRIMARY KEY (user_id, weekday, position),
    CONSTRAINT availability_weekly_intervals_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES availability_preferences (user_id) ON DELETE CASCADE,
    CONSTRAINT availability_weekly_intervals_weekday_check CHECK (weekday BETWEEN 1 AND 7),
    CONSTRAINT availability_weekly_intervals_position_check CHECK (position BETWEEN 0 AND 2),
    CONSTRAINT availability_weekly_intervals_minute_check CHECK (
        start_minute >= 0 AND end_minute <= 1440 AND start_minute < end_minute
        AND start_minute % 30 = 0 AND end_minute % 30 = 0
    )
);

-- +goose StatementBegin
CREATE FUNCTION availability_assert_no_interval_overlap() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    -- 같은 사용자의 동시 쓰기를 직렬화한다. 이 잠금 뒤의 문장은 새 snapshot으로 앞 커밋을 본다.
    PERFORM 1 FROM availability_preferences WHERE user_id = NEW.user_id FOR NO KEY UPDATE;
    -- 커밋 시점에는 row가 이미 지워졌거나 바뀌었을 수 있다. 지금 남아 있는 같은 요일 row 사이를 본다.
    -- 맞닿음(a.end = b.start)도 거부한다. service가 항상 병합하므로 맞닿은 row는 정규형이 아니다.
    IF EXISTS (
        SELECT 1
          FROM availability_weekly_intervals a
          JOIN availability_weekly_intervals b
            ON b.user_id = a.user_id AND b.weekday = a.weekday AND b.position <> a.position
         WHERE a.user_id = NEW.user_id AND a.weekday = NEW.weekday
           AND a.start_minute <= b.end_minute AND b.start_minute <= a.end_minute
    ) THEN
        RAISE EXCEPTION '같은 요일의 활동 구간이 겹치거나 맞닿는다'
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'availability_weekly_intervals_no_overlap_check';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- DELETE에는 걸지 않는다. 지우는 것으로 겹침이 생기지 않고, 사용자 삭제 cascade를 막지 않아야 한다.
CREATE CONSTRAINT TRIGGER availability_weekly_intervals_assert_no_overlap
    AFTER INSERT OR UPDATE ON availability_weekly_intervals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION availability_assert_no_interval_overlap();

-- +goose Down
DROP TABLE IF EXISTS availability_weekly_intervals;
DROP FUNCTION IF EXISTS availability_assert_no_interval_overlap();
DROP TABLE IF EXISTS availability_preferences;
