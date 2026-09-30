package party

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/mutation"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// projectionNamespace는 projection ID를 (party, owner, source_event_key)에서 결정적으로 만드는
// UUIDv5 namespace다. 같은 일정이 다시 만들어져도 클라이언트 캐시의 entity ID가 바뀌지 않는다.
var projectionNamespace = uuid.MustParse("6f1b7a2e-4c58-4a0d-9b3e-2d5a7c1e8f40")

func projectionID(partyID, ownerID uuid.UUID, key []byte) uuid.UUID {
	buf := make([]byte, 0, 32+len(key))
	buf = append(buf, partyID[:]...)
	buf = append(buf, ownerID[:]...)
	buf = append(buf, key...)
	return uuid.NewSHA1(projectionNamespace, buf)
}

// visibleStatuses는 다른 멤버에게 projection을 보여도 되는 연결 상태다. syncing과 error는
// 마지막 정상 데이터를 유지한다(설계 3 §5.2). denied·restricted·revoked·needs_source_reselection과
// 연결 전 상태는 즉시 비노출이다(§12).
var visibleStatuses = map[string]bool{"ready": true, "syncing": true, "error": true}

type busyFact struct {
	key      []byte
	start    time.Time
	end      time.Time
	allDay   bool
	timeZone string
}

type existingProjection struct {
	id     uuid.UUID
	key    []byte
	start  time.Time
	end    time.Time
	allDay bool
	tz     string
	level  string
}

// RebuildBusyProjections는 사용자의 활성 generation Busy facts로 사용자가 속한 Party들의 busy
// projection을 맞춘다(설계 4 §5.5.1, 설계 3 §8). 변경분은 활성 멤버 전원에게 upsert 또는
// tombstone으로 발행한다.
//
//   - onlyParty가 있으면 그 Party만 맞춘다(Party 생성·가입). nil이면 사용자의 모든 활성 Party다.
//   - hidden은 projection이 없다. details는 아직 상세를 올리지 않았으므로 busyOnly로 안전하게
//     강등한다(공개 수준 단계에서 details ciphertext가 추가된다).
//   - 연결이 없거나 visibleStatuses가 아니면 모든 projection을 지운다.
//
// 잠금: 이 함수가 calendar_connections 행을 FOR SHARE로 잡는다. connectionLocked가 true이면
// 호출자(snapshot complete)가 이미 FOR UPDATE로 잡고 있다. 테이블 잠금은 항상 parties →
// party_memberships 순으로 잡는다(스키마 테스트의 DDL 잠금과 같은 순서라 교착하지 않는다). connection은 다른 잠금 뒤에 마지막으로
// 잡아야 한다(migration 00012). 멤버십 행은 FOR KEY SHARE로 잡아 동시에 끝나는 멤버십에
// projection이 남지 않게 한다.
func RebuildBusyProjections(ctx context.Context, tx *mutation.Tx, userID uuid.UUID, onlyParty *uuid.UUID, connectionLocked bool) error {
	var (
		connID     uuid.UUID
		status     string
		generation int64
	)
	lockClause := "FOR SHARE"
	if connectionLocked {
		lockClause = ""
	}
	err := tx.QueryRow(ctx, `
		SELECT id, source_status, active_generation FROM calendar_connections
		 WHERE user_id = $1 `+lockClause, userID).Scan(&connID, &status, &generation)
	hasConn := err == nil
	if err != nil && !isNoRows(err) {
		return err
	}
	visible := hasConn && visibleStatuses[status] && generation > 0

	rows, err := tx.Query(ctx, `
		SELECT m.party_id, s.visibility_level, s.version
		  FROM parties p
		  JOIN party_memberships m ON m.party_id = p.id
		  JOIN party_visibility_settings s ON s.party_id = m.party_id AND s.user_id = m.user_id
		 WHERE p.status = 'active' AND m.user_id = $1 AND m.status = 'active'
		   AND ($2::uuid IS NULL OR m.party_id = $2)
		 ORDER BY m.party_id
		   FOR KEY SHARE OF m`, userID, onlyParty)
	if err != nil {
		return err
	}
	type target struct {
		partyID        uuid.UUID
		level          string
		settingVersion int64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.partyID, &t.level, &t.settingVersion); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	var facts []busyFact
	if visible {
		frows, err := tx.Query(ctx, `
			SELECT source_event_key, start_at, end_at, all_day, time_zone
			  FROM calendar_busy_facts WHERE connection_id = $1`, connID)
		if err != nil {
			return err
		}
		for frows.Next() {
			var f busyFact
			if err := frows.Scan(&f.key, &f.start, &f.end, &f.allDay, &f.timeZone); err != nil {
				frows.Close()
				return err
			}
			facts = append(facts, f)
		}
		frows.Close()
		if err := frows.Err(); err != nil {
			return err
		}
	}

	for _, t := range targets {
		desired := facts
		if !visible || t.level == "hidden" {
			desired = nil
		}
		if err := syncPartyProjections(ctx, tx, t.partyID, userID, desired, generation, t.settingVersion); err != nil {
			return err
		}
	}
	return nil
}

func syncPartyProjections(ctx context.Context, tx *mutation.Tx, partyID, ownerID uuid.UUID, desired []busyFact, generation, settingVersion int64) error {
	rows, err := tx.Query(ctx, `
		SELECT id, source_event_key, start_at, end_at, all_day, time_zone, visibility_level
		  FROM party_schedule_projections
		 WHERE party_id = $1 AND owner_user_id = $2`, partyID, ownerID)
	if err != nil {
		return err
	}
	existing := map[string]existingProjection{}
	for rows.Next() {
		var e existingProjection
		if err := rows.Scan(&e.id, &e.key, &e.start, &e.end, &e.allDay, &e.tz, &e.level); err != nil {
			rows.Close()
			return err
		}
		existing[hex.EncodeToString(e.key)] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var upserts []busyFact
	unchanged := make([][]byte, 0, len(desired))
	seen := make(map[string]struct{}, len(desired))
	for _, f := range desired {
		k := hex.EncodeToString(f.key)
		seen[k] = struct{}{}
		e, ok := existing[k]
		if ok && e.start.Equal(f.start) && e.end.Equal(f.end) && e.allDay == f.allDay && e.tz == f.timeZone && e.level == "busyOnly" {
			unchanged = append(unchanged, f.key)
			continue
		}
		upserts = append(upserts, f)
	}
	var stale [][]byte
	for k, e := range existing {
		if _, ok := seen[k]; !ok {
			stale = append(stale, e.key)
		}
	}
	if len(upserts) == 0 && len(stale) == 0 && len(unchanged) == 0 {
		return nil
	}

	recipients, err := activeMemberIDs(ctx, tx, partyID)
	if err != nil {
		return err
	}

	if len(stale) > 0 {
		drows, err := tx.Query(ctx, `
			DELETE FROM party_schedule_projections
			 WHERE party_id = $1 AND owner_user_id = $2 AND source_event_key = ANY($3)
			RETURNING id`, partyID, ownerID, stale)
		if err != nil {
			return err
		}
		var ids []uuid.UUID
		for drows.Next() {
			var id uuid.UUID
			if err := drows.Scan(&id); err != nil {
				drows.Close()
				return err
			}
			ids = append(ids, id)
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			return err
		}
		changes := make([]mutation.Change, 0, len(ids)*len(recipients))
		for _, id := range ids {
			for _, r := range recipients {
				changes = append(changes, mutation.Change{Recipient: r, EntityType: entityScheduleProjection, EntityID: id})
			}
		}
		if err := tx.EmitChanges(ctx, changes); err != nil {
			return err
		}
	}

	if len(upserts) > 0 {
		ids := make([]uuid.UUID, len(upserts))
		keys := make([][]byte, len(upserts))
		starts := make([]time.Time, len(upserts))
		ends := make([]time.Time, len(upserts))
		allDays := make([]bool, len(upserts))
		zones := make([]string, len(upserts))
		for i, f := range upserts {
			ids[i], keys[i], starts[i], ends[i], allDays[i], zones[i] = projectionID(partyID, ownerID, f.key), f.key, f.start, f.end, f.allDay, f.timeZone
		}
		urows, err := tx.Query(ctx, `
			INSERT INTO party_schedule_projections AS p
			       (id, party_id, owner_user_id, source_event_key, source_generation, setting_version,
			        visibility_level, start_at, end_at, all_day, time_zone)
			SELECT t.id, $1, $2, t.key, $3, $4, 'busyOnly', t.start_at, t.end_at, t.all_day, t.tz
			  FROM unnest($5::uuid[], $6::bytea[], $7::timestamptz[], $8::timestamptz[], $9::boolean[], $10::text[])
			       AS t(id, key, start_at, end_at, all_day, tz)
			ON CONFLICT (party_id, owner_user_id, source_event_key) DO UPDATE
			   SET source_generation = EXCLUDED.source_generation,
			       setting_version   = EXCLUDED.setting_version,
			       visibility_level  = 'busyOnly',
			       start_at = EXCLUDED.start_at, end_at = EXCLUDED.end_at,
			       all_day = EXCLUDED.all_day, time_zone = EXCLUDED.time_zone,
			       title_ciphertext = NULL, location_ciphertext = NULL,
			       version = p.version + 1, updated_at = now()
			RETURNING id, version, start_at, end_at, all_day, time_zone`,
			partyID, ownerID, generation, settingVersion, ids, keys, starts, ends, allDays, zones)
		if err != nil {
			return err
		}
		type out struct {
			id      uuid.UUID
			version int64
			start   time.Time
			end     time.Time
			allDay  bool
			tz      string
		}
		var outs []out
		for urows.Next() {
			var o out
			if err := urows.Scan(&o.id, &o.version, &o.start, &o.end, &o.allDay, &o.tz); err != nil {
				urows.Close()
				return err
			}
			outs = append(outs, o)
		}
		urows.Close()
		if err := urows.Err(); err != nil {
			return err
		}
		changes := make([]mutation.Change, 0, len(outs)*len(recipients))
		for _, o := range outs {
			version := o.version
			payload := map[string]any{
				"party_id": partyID.String(), "owner_user_id": ownerID.String(), "visibility_level": "busyOnly",
				"start_at": o.start.UTC().Format(time.RFC3339), "end_at": o.end.UTC().Format(time.RFC3339),
				"all_day": o.allDay, "time_zone": o.tz,
			}
			for _, r := range recipients {
				changes = append(changes, mutation.Change{Recipient: r, EntityType: entityScheduleProjection, EntityID: o.id, Version: &version, Payload: payload})
			}
		}
		if err := tx.EmitChanges(ctx, changes); err != nil {
			return err
		}
	}

	// 바뀌지 않은 row도 source generation과 setting version을 맞춘다. version은 올리지 않는다
	// (클라이언트에 보이는 값이 바뀌지 않았다).
	if len(unchanged) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE party_schedule_projections
			   SET source_generation = $3, setting_version = $4
			 WHERE party_id = $1 AND owner_user_id = $2 AND source_event_key = ANY($5)
			   AND (source_generation <> $3 OR setting_version <> $4)`,
			partyID, ownerID, generation, settingVersion, unchanged); err != nil {
			return err
		}
	}
	return nil
}

// emitPartyProjectionsTo는 Party의 projection을 recipient에게 upsert로 발행한다. excludeOwner의
// projection은 제외한다. 신규 멤버 부트스트랩에서 쓴다. payload는 시간과 종일 여부·시간대뿐이다.
func emitPartyProjectionsTo(ctx context.Context, tx *mutation.Tx, recipient, partyID, excludeOwner uuid.UUID) error {
	rows, err := tx.Query(ctx, `
		SELECT id, owner_user_id, visibility_level, start_at, end_at, all_day, time_zone, version
		  FROM party_schedule_projections
		 WHERE party_id = $1 AND owner_user_id <> $2
		 ORDER BY owner_user_id, start_at, id`, partyID, excludeOwner)
	if err != nil {
		return err
	}
	type row struct {
		id, owner uuid.UUID
		level     string
		start     time.Time
		end       time.Time
		allDay    bool
		tz        string
		version   int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.owner, &r.level, &r.start, &r.end, &r.allDay, &r.tz, &r.version); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	changes := make([]mutation.Change, 0, len(all))
	for _, r := range all {
		version := r.version
		payload := map[string]any{
			"party_id": partyID.String(), "owner_user_id": r.owner.String(), "visibility_level": r.level,
			"start_at": r.start.UTC().Format(time.RFC3339), "end_at": r.end.UTC().Format(time.RFC3339),
			"all_day": r.allDay, "time_zone": r.tz,
		}
		changes = append(changes, mutation.Change{Recipient: recipient, EntityType: entityScheduleProjection, EntityID: r.id, Version: &version, Payload: payload})
	}
	return tx.EmitChanges(ctx, changes)
}

// LockUserPartiesShared는 사용자가 속한 활성 Party 행을 id 순서로 FOR SHARE로 잠근다. snapshot
// complete와 연결 상태 변경이 connection을 잡기 전에 호출한다. 그러면 (1) 같은 Party에 가입하는
// T3(parties FOR UPDATE)와 직렬화돼 신규 멤버가 수신자에서 빠지거나 projection 부트스트랩을
// 낡은 상태로 받는 경합이 없고, (2) 잠금 순서가 parties -> memberships -> connection으로 다른
// Party 트랜잭션과 같다. 잠금은 트랜잭션이 끝날 때 풀린다.
func LockUserPartiesShared(ctx context.Context, tx *mutation.Tx, userID uuid.UUID) error {
	rows, err := tx.Query(ctx, `
		SELECT p.id
		  FROM parties p
		  JOIN party_memberships m ON m.party_id = p.id
		 WHERE m.user_id = $1 AND m.status = 'active' AND p.status = 'active'
		 ORDER BY p.id
		   FOR SHARE OF p`, userID)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}
