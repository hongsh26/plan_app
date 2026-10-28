package availability

// A3: 검색 입력 snapshot, freshness gate, calculationVersion(설계 6 §5.1, §6, §7.2, §9).
//
// 한 검색 요청은 REPEATABLE READ READ ONLY 트랜잭션 하나에서 Party·활성 멤버십·활동 시간·캘린더 연결과
// active generation window·Busy facts를 읽는다. 준비되지 않은 멤버가 한 명이라도 있으면 계산하지 않고
// aggregate 상태만 돌려준다. 다른 멤버의 수·ID·원인은 오류에도 싣지 않는다.

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/availability/engine"
	"plantogether/server/internal/calendar"
)

const (
	// 409 aggregate 상태 code(§6, §10).
	CodeSetupPending  = "availability_setup_pending"
	CodeSyncPending   = "calendar_sync_pending"
	CodeChangedRetry  = "availability_changed_retry"
	CodeResultStale   = "availability_result_stale"
	CodeRangeInvalid  = "search_range_invalid"
	CodeUnavailable   = "availability_temporarily_unavailable"
	versionKeyInfo    = "plantogether/availability-calculation-version/v1"
	versionFormat     = byte(1)
	resultValidity    = 5 * time.Minute
	pgSerializationEr = "40001"
)

// self.calendar 값. 호출자 본인의 캘린더 상태만 이 값으로 알려 준다(§6 "본인용 CTA").
const (
	SelfCalendarReady        = "ready"
	SelfCalendarNotConnected = "not_connected"
	SelfCalendarDenied       = "denied"
	SelfCalendarRevoked      = "revoked"
	SelfCalendarSyncing      = "syncing"
	SelfCalendarSourceNeeded = "source_required"
	SelfCalendarError        = "error"
	SelfCalendarStale        = "stale"
)

var (
	errSearchNotFound = errors.New("availability: Party가 없거나 활성 멤버가 아니다")
	// errRangeNotCovered는 요청 envelope가 모든 active generation window 교집합 밖으로 나간 경우다.
	errRangeNotCovered = errors.New("availability: 요청 범위가 캘린더 snapshot 범위를 벗어난다")
	// errChangedRetry는 serialization failure가 재시도 뒤에도 반복된 경우다.
	errChangedRetry = errors.New("availability: 검색 입력이 계산 중 바뀌었다")
	// ErrResultStale은 calculationVersion이 현재 입력과 다르거나 validUntil이 지난 경우다.
	// 설계 7은 이 오류를 409 availability_result_stale로 응답한다.
	ErrResultStale = errors.New("availability: 검색 결과가 만료됐거나 입력이 바뀌었다")
)

// SelfState는 호출자 본인의 준비 상태다. 다른 멤버 상태는 담지 않는다.
type SelfState struct {
	Activity string `json:"activity"`
	Calendar string `json:"calendar"`
}

// PendingError는 준비 미완료 aggregate 상태다. Code는 CodeSetupPending 또는 CodeSyncPending이다.
type PendingError struct {
	Code string
	Self SelfState
}

func (e *PendingError) Error() string {
	return "availability: 검색 준비 미완료 (" + e.Code + ")"
}

// SearchParams는 기본값을 채운 뒤의 검색 조건이다. version tuple의 search_parameters_hash 입력이다.
// 설계 7은 검색 응답을 만든 것과 같은 값(날짜를 명시한 값)을 넘겨야 한다.
type SearchParams struct {
	StartDate       engine.Date
	EndDate         engine.Date
	DurationMinutes int
	TimeZone        string
	MaxResults      int
}

type memberRow struct {
	userID        uuid.UUID
	membershipID  uuid.UUID
	membershipVer int64
}

type connRow struct {
	connectionID uuid.UUID
	status       string
	lastSync     *time.Time
	generation   int64
	windowStart  *time.Time
	windowEnd    *time.Time
}

// searchState는 facts를 제외한 snapshot이다. 검증(VerifyCalculationVersion)은 이것만 읽는다.
type searchState struct {
	members    []memberRow // user_id 오름차순
	prefs      map[uuid.UUID]Preference
	conns      map[uuid.UUID]connRow
	freshUntil time.Time
	canonical  []byte
}

// searchSnapshot은 계산 입력 전체다.
type searchSnapshot struct {
	state   *searchState
	members []engine.Member
}

// readSearchState는 Party·멤버십·활동 시간·캘린더 연결을 읽고 gate를 판정한다.
//
// 판정 순서: Party·호출자 멤버십(404) → 활동 시간 미설정(409 setup) → 캘린더 비정상·stale·window 없음
// (409 sync) → envelope 포함(400 range). 활동 시간과 캘린더가 모두 미준비면 setup이 우선한다.
// 활동 시간은 앱 안에서 바로 고칠 수 있고 캘린더 재동기화보다 먼저 요구하는 단계이기 때문이다(§3.1).
func readSearchState(ctx context.Context, q Querier, partyID, callerID uuid.UUID, p SearchParams, env engine.Interval, now time.Time) (*searchState, error) {
	var partyVersion int64
	var callerActive bool
	found := false
	rows, err := q.Query(ctx, `
		SELECT p.version,
		       EXISTS (SELECT 1 FROM party_memberships m
		                WHERE m.party_id = p.id AND m.user_id = $2 AND m.status = 'active')
		  FROM parties p
		 WHERE p.id = $1 AND p.status = 'active'`, partyID, callerID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		found = true
		if err := rows.Scan(&partyVersion, &callerActive); err != nil {
			rows.Close()
			return nil, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 없는 Party, 해산된 Party, 비멤버, 떠난 멤버를 구분하지 않는다.
	if !found || !callerActive {
		return nil, errSearchNotFound
	}

	st := &searchState{conns: map[uuid.UUID]connRow{}}
	rows, err = q.Query(ctx, `
		SELECT user_id, id, version FROM party_memberships
		 WHERE party_id = $1 AND status = 'active'`, partyID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.userID, &m.membershipID, &m.membershipVer); err != nil {
			rows.Close()
			return nil, err
		}
		st.members = append(st.members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(st.members, func(i, j int) bool {
		return bytes.Compare(st.members[i].userID[:], st.members[j].userID[:]) < 0
	})
	ids := make([]uuid.UUID, len(st.members))
	for i, m := range st.members {
		ids[i] = m.userID
	}

	if st.prefs, err = LoadPreferences(ctx, q, ids); err != nil {
		return nil, err
	}

	// active generation의 window는 그 generation을 만든 completed session에만 있다.
	rows, err = q.Query(ctx, `
		SELECT c.user_id, c.id, c.source_status, c.last_completed_sync_at, c.active_generation,
		       s.window_start, s.window_end
		  FROM calendar_connections c
		  LEFT JOIN LATERAL (
		        SELECT window_start, window_end FROM calendar_snapshot_sessions
		         WHERE connection_id = c.id AND status = 'completed' AND generation = c.active_generation
		         LIMIT 1) s ON c.active_generation > 0
		 WHERE c.user_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var uid uuid.UUID
		var c connRow
		if err := rows.Scan(&uid, &c.connectionID, &c.status, &c.lastSync, &c.generation, &c.windowStart, &c.windowEnd); err != nil {
			rows.Close()
			return nil, err
		}
		st.conns[uid] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var self SelfState
	setupPending, syncPending := false, false
	for _, m := range st.members {
		activity := StatusConfigured
		if st.prefs[m.userID].Status != StatusConfigured {
			activity = StatusUnconfigured
			setupPending = true
		}
		c, ok := st.conns[m.userID]
		cal := calendarState(c, ok, now)
		if cal != SelfCalendarReady {
			syncPending = true
		}
		if m.userID == callerID {
			self = SelfState{Activity: activity, Calendar: cal}
		}
	}
	switch {
	case setupPending:
		return nil, &PendingError{Code: CodeSetupPending, Self: self}
	case syncPending:
		return nil, &PendingError{Code: CodeSyncPending, Self: self}
	}

	// 모든 active generation window의 UTC 교집합이 envelope 전체를 덮어야 한다. 누락 구간을 free로
	// 해석하지 않는다(§4). 어느 멤버가 부족한지는 알리지 않는다.
	for i, m := range st.members {
		c := st.conns[m.userID]
		if c.windowStart.After(env.Start) || c.windowEnd.Before(env.End) {
			return nil, errRangeNotCovered
		}
		until := c.lastSync.Add(calendar.FreshnessWindow)
		if i == 0 || until.Before(st.freshUntil) {
			st.freshUntil = until
		}
	}
	st.canonical = canonicalTuple(partyID, partyVersion, st, p)
	return st, nil
}

// calendarState는 한 멤버의 캘린더 준비 상태다. stale은 저장하지 않고 now로 계산한다
// (calendar.FreshUsers와 같은 기준: last_completed_sync_at + 6h > now이어야 fresh).
// ready여도 active generation의 window row가 없으면(session 24시간 정리, calendar.Prune) 범위를 확인할 수
// 없으므로 stale로 본다. 재동기화가 새 window를 만든다.
func calendarState(c connRow, exists bool, now time.Time) string {
	if !exists {
		return SelfCalendarNotConnected
	}
	switch c.status {
	case "ready":
		if c.lastSync == nil || !c.lastSync.Add(calendar.FreshnessWindow).After(now) ||
			c.windowStart == nil || c.windowEnd == nil {
			return SelfCalendarStale
		}
		return SelfCalendarReady
	case "disconnected":
		return SelfCalendarNotConnected
	case "permission_required", "denied", "restricted":
		return SelfCalendarDenied
	case "revoked":
		return SelfCalendarRevoked
	case "selecting", "syncing":
		return SelfCalendarSyncing
	case "needs_source_reselection":
		return SelfCalendarSourceNeeded
	default:
		return SelfCalendarError
	}
}

// readSearchSnapshot은 state와 envelope에 겹치는 active generation Busy facts를 읽는다.
func readSearchSnapshot(ctx context.Context, q Querier, partyID, callerID uuid.UUID, p SearchParams, env engine.Interval, now time.Time) (*searchSnapshot, error) {
	st, err := readSearchState(ctx, q, partyID, callerID, p, env, now)
	if err != nil {
		return nil, err
	}
	connIDs := make([]uuid.UUID, len(st.members))
	gens := make([]int64, len(st.members))
	for i, m := range st.members {
		connIDs[i] = st.conns[m.userID].connectionID
		gens[i] = st.conns[m.userID].generation
	}
	busy := map[uuid.UUID][]engine.Interval{}
	// active generation의 facts만 읽는다. availability 값(busy/tentative/unavailable)은 모두 Busy다.
	rows, err := q.Query(ctx, `
		SELECT f.user_id, f.start_at, f.end_at
		  FROM calendar_busy_facts f
		  JOIN unnest($1::uuid[], $2::bigint[]) AS a(connection_id, generation)
		    ON f.connection_id = a.connection_id AND f.generation = a.generation
		 WHERE f.start_at < $4 AND f.end_at > $3`, connIDs, gens, env.Start, env.End)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var uid uuid.UUID
		var iv engine.Interval
		if err := rows.Scan(&uid, &iv.Start, &iv.End); err != nil {
			rows.Close()
			return nil, err
		}
		busy[uid] = append(busy[uid], engine.Interval{Start: iv.Start.UTC(), End: iv.End.UTC()})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	snap := &searchSnapshot{state: st, members: make([]engine.Member, len(st.members))}
	for i, m := range st.members {
		pref := st.prefs[m.userID]
		mloc, err := time.LoadLocation(pref.TimeZone)
		if err != nil {
			return nil, err
		}
		snap.members[i] = engine.Member{Location: mloc, Weekly: pref.Intervals, Busy: busy[m.userID]}
	}
	return snap, nil
}

// readSnapshotTx는 REPEATABLE READ READ ONLY 트랜잭션 하나로 snapshot을 읽는다. serialization failure는
// 한 번 다시 시도하고, 다시 실패하면 errChangedRetry다(§5.1).
func readSnapshotTx(ctx context.Context, pool *pgxpool.Pool, partyID, callerID uuid.UUID, p SearchParams, env engine.Interval, now time.Time) (*searchSnapshot, error) {
	var snap *searchSnapshot
	err := withSerializationRetry(func() error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		s, err := readSearchSnapshot(ctx, tx, partyID, callerID, p, env, now)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		snap = s
		return nil
	})
	return snap, err
}

// withSerializationRetry는 fn이 SQLSTATE 40001로 실패하면 한 번 더 실행한다. 두 번째도 40001이면
// errChangedRetry다. READ ONLY REPEATABLE READ 트랜잭션은 보통 serialization failure를 내지 않으므로
// 이 경로는 방어용이다(실제 DB에서 재현하지 않고 주입 오류로 단위 테스트한다).
func withSerializationRetry(fn func() error) error {
	for attempt := 0; attempt < 2; attempt++ {
		err := fn()
		if !isSerializationFailure(err) {
			return err
		}
	}
	return errChangedRetry
}

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgSerializationEr
}

// canonicalTuple은 version tuple(§5.1)의 결정적 인코딩이다. 모든 가변 길이 값은 길이를 앞에 붙인다.
//
//	party_id, party_version,
//	membership: (user_id, membership_id, membership_version) user_id 오름차순,
//	activity:   (user_id, preference version),
//	calendar:   (user_id, connection_id, active_generation),
//	fresh_until(UTC 마이크로초),
//	params:     start_date, end_date, duration_minutes, time_zone, max_results
//
// 세 hash를 따로 만들지 않고 한 인코딩에 모두 넣어 HMAC 하나로 묶는다. 어느 요소가 바뀌어도 결과가 바뀐다.
func canonicalTuple(partyID uuid.UUID, partyVersion int64, st *searchState, p SearchParams) []byte {
	var b bytes.Buffer
	putStr := func(s string) {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(s)))
		b.WriteString(s)
	}
	putInt := func(n int64) { _ = binary.Write(&b, binary.BigEndian, n) }
	putStr("availability-tuple/v1")
	b.Write(partyID[:])
	putInt(partyVersion)
	putInt(int64(len(st.members)))
	for _, m := range st.members {
		b.Write(m.userID[:])
		b.Write(m.membershipID[:])
		putInt(m.membershipVer)
	}
	for _, m := range st.members {
		b.Write(m.userID[:])
		putInt(st.prefs[m.userID].Version)
	}
	for _, m := range st.members {
		c := st.conns[m.userID]
		b.Write(m.userID[:])
		b.Write(c.connectionID[:])
		putInt(c.generation)
	}
	putInt(st.freshUntil.UTC().UnixMicro())
	putStr(dateString(p.StartDate))
	putStr(dateString(p.EndDate))
	putInt(int64(p.DurationMinutes))
	putStr(p.TimeZone)
	putInt(int64(p.MaxResults))
	return b.Bytes()
}

func dateString(d engine.Date) string {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
}

// Versioner는 calculationVersion을 만들고 검증한다. 키는 RATE_LIMIT_KEY에서 HKDF-SHA256(info =
// versionKeyInfo)로 파생한다. RATE_LIMIT_KEY를 교체하면 남은 version이 모두 stale이 되지만 유효기간이
// 최대 5분이라 허용한다.
type Versioner struct {
	key []byte
}

// NewVersioner는 secret(RATE_LIMIT_KEY 원문 바이트)에서 version 키를 파생한다.
func NewVersioner(secret []byte) (*Versioner, error) {
	if len(secret) == 0 {
		return nil, errors.New("availability: calculationVersion secret이 비어 있다")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, versionKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	return &Versioner{key: key}, nil
}

// token 형식: base64url(format 1바이트 ‖ validUntil Unix 초 8바이트 ‖ HMAC-SHA256(key, canonical ‖ validUntil)).
// validUntil을 MAC 안에 넣어 서버 상태 없이 만료를 검사하고, 클라이언트가 바꾸지 못하게 한다.
// 내부 ID와 version은 HMAC 안에만 있어 token에서 복원할 수 없다.
func (v *Versioner) token(canonical []byte, validUntil time.Time) string {
	var tail [8]byte
	binary.BigEndian.PutUint64(tail[:], uint64(validUntil.Unix()))
	raw := append([]byte{versionFormat}, tail[:]...)
	raw = append(raw, v.mac(canonical, tail[:])...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (v *Versioner) mac(canonical, tail []byte) []byte {
	m := hmac.New(sha256.New, v.key)
	m.Write(canonical)
	m.Write(tail)
	return m.Sum(nil)
}

// VerifyCalculationVersion은 설계 7의 제안 생성 경계가 부른다. 현재 snapshot의 tuple이 version을 만든
// tuple과 같고 now < validUntil이어야 nil이다.
//
//   - 비멤버·없는 Party: 검색과 같은 not-found 오류(errSearchNotFound, IsNotFound로 판별)
//   - tuple 불일치, 만료, 형식 오류, 준비 미완료·범위 미포함: ErrResultStale(→ 409 availability_result_stale)
//
// q는 호출자 트랜잭션이어도 된다. facts는 읽지 않는다.
func (v *Versioner) VerifyCalculationVersion(ctx context.Context, q Querier, partyID, callerID uuid.UUID, p SearchParams, version string, now time.Time) error {
	raw, err := base64.RawURLEncoding.DecodeString(version)
	if err != nil || len(raw) != 1+8+sha256.Size || raw[0] != versionFormat {
		return ErrResultStale
	}
	validUntil := time.Unix(int64(binary.BigEndian.Uint64(raw[1:9])), 0)
	if !now.Before(validUntil) {
		return ErrResultStale
	}
	loc, err := time.LoadLocation(p.TimeZone)
	if err != nil || !ValidTimeZone(p.TimeZone) {
		return ErrResultStale
	}
	st, err := readSearchState(ctx, q, partyID, callerID, p, engine.RequestEnvelope(p.StartDate, p.EndDate, loc), now)
	var pending *PendingError
	switch {
	case errors.Is(err, errSearchNotFound):
		return err
	case errors.As(err, &pending), errors.Is(err, errRangeNotCovered):
		return ErrResultStale
	case err != nil:
		return err
	}
	if !hmac.Equal(raw[9:], v.mac(st.canonical, raw[1:9])) {
		return ErrResultStale
	}
	return nil
}

// IsNotFound는 VerifyCalculationVersion의 not-found 오류인지 판별한다.
func IsNotFound(err error) bool { return errors.Is(err, errSearchNotFound) }
