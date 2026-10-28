package party

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/platform/mutation"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

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

type desiredProjection struct {
	busyFact
	id                 uuid.UUID
	level              string
	titlePlaintext     []byte
	locationPlaintext  []byte
	titleCiphertext    []byte
	locationCiphertext []byte
}

type existingProjection struct {
	id       uuid.UUID
	key      []byte
	start    time.Time
	end      time.Time
	allDay   bool
	tz       string
	level    string
	title    []byte
	location []byte
	version  int64
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
	return RebuildBusyProjectionsWithDetails(ctx, tx, userID, onlyParty, connectionLocked, nil, nil)
}

// RebuildBusyProjectionsWithDetails는 snapshot complete 경로에서만 details allowlist를 projection으로
// 승격한다. details가 없거나 설정이 details가 아니면 기존처럼 busyOnly로 안전 강등한다.
func RebuildBusyProjectionsWithDetails(ctx context.Context, tx *mutation.Tx, userID uuid.UUID, onlyParty *uuid.UUID, connectionLocked bool, box DetailBox, details map[string]ProjectionPlainDetails) error {
	var (
		connID     uuid.UUID
		status     string
		generation int64
		usable     bool
	)
	lockClause := "FOR SHARE"
	if connectionLocked {
		lockClause = ""
	}
	err := tx.QueryRow(ctx, `
		SELECT id, source_status, active_generation, active_generation_usable FROM calendar_connections
		 WHERE user_id = $1 `+lockClause, userID).Scan(&connID, &status, &generation, &usable)
	hasConn := err == nil
	if err != nil && !isNoRows(err) {
		return err
	}
	// 노출 상태여도 active generation을 쓸 수 없으면(철회·source 변경 뒤 새 complete 전) 비노출이다
	// (migration 00017, 설계 3 §5.2·§12의 "fresh 해제·projection 비노출" 보완).
	visible := hasConn && visibleStatuses[status] && generation > 0 && usable

	rows, err := tx.Query(ctx, `
		SELECT m.party_id, s.visibility_level, s.share_location, s.version
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
		shareLocation  bool
		settingVersion int64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.partyID, &t.level, &t.shareLocation, &t.settingVersion); err != nil {
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
		desired, err := buildDesiredProjections(t.level, t.shareLocation, facts, details)
		if err != nil {
			return err
		}
		if !visible || t.level == "hidden" {
			desired = nil
		}
		if err := syncPartyProjections(ctx, tx, t.partyID, userID, desired, generation, t.settingVersion, box, details == nil && t.level == visibilityDetails, t.shareLocation); err != nil {
			return err
		}
	}
	return nil
}

func buildDesiredProjections(level string, shareLocation bool, facts []busyFact, details map[string]ProjectionPlainDetails) ([]desiredProjection, error) {
	out := make([]desiredProjection, 0, len(facts))
	for _, f := range facts {
		d := desiredProjection{busyFact: f, level: "busyOnly"}
		if level == "details" {
			if pd, ok := details[string(f.key)]; ok && pd.HasDetails {
				d.level = "details"
				d.titlePlaintext = pd.TitlePlaintext
				if shareLocation {
					d.locationPlaintext = pd.LocationPlaintext
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func projectionPlaintextMatches(box DetailBox, sealed, plain []byte, purpose string) bool {
	if len(sealed) == 0 || len(plain) == 0 {
		return len(sealed) == 0 && len(plain) == 0
	}
	opened, err := box.Open(sealed, purpose)
	return err == nil && bytes.Equal(opened, plain)
}

func syncPartyProjections(ctx context.Context, tx *mutation.Tx, partyID, ownerID uuid.UUID, desired []desiredProjection, generation, settingVersion int64, box DetailBox, preserveExistingDetails, shareLocation bool) error {
	rows, err := tx.Query(ctx, `
		SELECT id, source_event_key, start_at, end_at, all_day, time_zone, visibility_level, title_ciphertext, location_ciphertext, version
		  FROM party_schedule_projections
		 WHERE party_id = $1 AND owner_user_id = $2`, partyID, ownerID)
	if err != nil {
		return err
	}
	existing := map[string]existingProjection{}
	for rows.Next() {
		var e existingProjection
		if err := rows.Scan(&e.id, &e.key, &e.start, &e.end, &e.allDay, &e.tz, &e.level, &e.title, &e.location, &e.version); err != nil {
			rows.Close()
			return err
		}
		existing[hex.EncodeToString(e.key)] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var upserts []desiredProjection
	unchanged := make([][]byte, 0, len(desired))
	seen := make(map[string]struct{}, len(desired))
	for _, f := range desired {
		k := hex.EncodeToString(f.key)
		seen[k] = struct{}{}
		e, ok := existing[k]
		preserved := preserveExistingDetails && ok && e.level == visibilityDetails
		if preserved {
			f.level, f.titleCiphertext = visibilityDetails, e.title
			if shareLocation {
				f.locationCiphertext = e.location
			}
		}
		if f.level == visibilityDetails && box == nil && !preserved {
			return errors.New("party: details projection sealer가 없다")
		}
		f.id = uuid.New()
		if ok {
			f.id = e.id
		}
		titleMatches, locationMatches := false, false
		if preserved {
			titleMatches = true
			locationMatches = bytes.Equal(e.location, f.locationCiphertext)
		} else if f.level == visibilityDetails && ok && e.level == visibilityDetails {
			titleMatches = projectionPlaintextMatches(box, e.title, f.titlePlaintext, SealPurposeProjectionTitle(e.id))
			locationMatches = projectionPlaintextMatches(box, e.location, f.locationPlaintext, SealPurposeProjectionLocation(e.id))
		} else if f.level == visibilityBusyOnly {
			titleMatches, locationMatches = e.title == nil, e.location == nil
		}
		if ok && e.start.Equal(f.start) && e.end.Equal(f.end) && e.allDay == f.allDay && e.tz == f.timeZone &&
			e.level == f.level && titleMatches && locationMatches {
			unchanged = append(unchanged, f.key)
			continue
		}
		if f.level == visibilityDetails && !preserved {
			var err error
			if len(f.titlePlaintext) > 0 {
				f.titleCiphertext, err = box.Seal(f.titlePlaintext, SealPurposeProjectionTitle(f.id))
				if err != nil {
					return err
				}
			}
			if len(f.locationPlaintext) > 0 {
				f.locationCiphertext, err = box.Seal(f.locationPlaintext, SealPurposeProjectionLocation(f.id))
				if err != nil {
					return err
				}
			}
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
			RETURNING id, version`, partyID, ownerID, stale)
		if err != nil {
			return err
		}
		type deletedProjection struct {
			id      uuid.UUID
			version int64
		}
		var deleted []deletedProjection
		for drows.Next() {
			var d deletedProjection
			if err := drows.Scan(&d.id, &d.version); err != nil {
				drows.Close()
				return err
			}
			deleted = append(deleted, d)
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			return err
		}
		changes := make([]mutation.Change, 0, len(deleted)*len(recipients))
		for _, d := range deleted {
			version := d.version + 1
			for _, r := range recipients {
				changes = append(changes, mutation.Change{Recipient: r, EntityType: entityScheduleProjection, EntityID: d.id, Version: &version, Tombstone: true})
			}
		}
		if err := emitScoped(ctx, tx, partyID, changes...); err != nil {
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
		levels := make([]string, len(upserts))
		titles := make([][]byte, len(upserts))
		locations := make([][]byte, len(upserts))
		for i, f := range upserts {
			ids[i], keys[i], starts[i], ends[i], allDays[i], zones[i] = f.id, f.key, f.start, f.end, f.allDay, f.timeZone
			levels[i], titles[i], locations[i] = f.level, f.titleCiphertext, f.locationCiphertext
		}
		urows, err := tx.Query(ctx, `
			INSERT INTO party_schedule_projections AS p
			       (id, party_id, owner_user_id, source_event_key, source_generation, setting_version,
			        visibility_level, start_at, end_at, all_day, time_zone, title_ciphertext, location_ciphertext)
			SELECT t.id, $1, $2, t.key, $3, $4, t.level, t.start_at, t.end_at, t.all_day, t.tz, t.title_ciphertext, t.location_ciphertext
			  FROM unnest($5::uuid[], $6::bytea[], $7::timestamptz[], $8::timestamptz[], $9::boolean[], $10::text[], $11::text[], $12::bytea[], $13::bytea[])
			       AS t(id, key, start_at, end_at, all_day, tz, level, title_ciphertext, location_ciphertext)
			ON CONFLICT (party_id, owner_user_id, source_event_key) DO UPDATE
			   SET source_generation = EXCLUDED.source_generation,
			       setting_version   = EXCLUDED.setting_version,
			       visibility_level  = EXCLUDED.visibility_level,
			       start_at = EXCLUDED.start_at, end_at = EXCLUDED.end_at,
			       all_day = EXCLUDED.all_day, time_zone = EXCLUDED.time_zone,
			       title_ciphertext = EXCLUDED.title_ciphertext, location_ciphertext = EXCLUDED.location_ciphertext,
			       version = p.version + 1, updated_at = now()
			RETURNING id, version, visibility_level, start_at, end_at, all_day, time_zone`,
			partyID, ownerID, generation, settingVersion, ids, keys, starts, ends, allDays, zones, levels, titles, locations)
		if err != nil {
			return err
		}
		type out struct {
			id      uuid.UUID
			version int64
			level   string
			start   time.Time
			end     time.Time
			allDay  bool
			tz      string
		}
		var outs []out
		for urows.Next() {
			var o out
			if err := urows.Scan(&o.id, &o.version, &o.level, &o.start, &o.end, &o.allDay, &o.tz); err != nil {
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
				"party_id": partyID.String(), "owner_user_id": ownerID.String(), "visibility_level": o.level,
				"start_at": o.start.UTC().Format(time.RFC3339), "end_at": o.end.UTC().Format(time.RFC3339),
				"all_day": o.allDay, "time_zone": o.tz,
			}
			for _, r := range recipients {
				changes = append(changes, mutation.Change{Recipient: r, EntityType: entityScheduleProjection, EntityID: o.id, Version: &version, Payload: payload})
			}
		}
		if err := emitScoped(ctx, tx, partyID, changes...); err != nil {
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
	return emitScoped(ctx, tx, partyID, changes...)
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
