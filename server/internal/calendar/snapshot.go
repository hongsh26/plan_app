package calendar

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/party"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/ratelimit"
)

// 한 요청 본문이 64KB로 제한되므로(httpapi.ReadBody) page당 fact를 250개로 둔다. fact 하나는
// 200바이트 안팎이다.
const (
	maxFactsPerPage   = 250
	maxPages          = 200
	maxTotalFacts     = 20000
	maxWindow         = 100 * 24 * time.Hour
	maxFactDuration   = 366 * 24 * time.Hour
	minKeyBytes       = 16
	maxKeyBytes       = 32
	codeSnapshotState = "calendar_snapshot_state"
)

var errSnapshotState = errors.New("calendar: snapshot session 상태가 이 동작을 허용하지 않는다")

type snapshotResponse struct {
	ID            uuid.UUID `json:"id"`
	Revision      int64     `json:"revision"`
	WindowStart   string    `json:"window_start"`
	WindowEnd     string    `json:"window_end"`
	ExpectedPages int       `json:"expected_pages"`
	Status        string    `json:"status"`
	FactCount     *int      `json:"fact_count"`
	Generation    *int64    `json:"generation"`
}

type snapshotEnvelope struct {
	RequestID  string             `json:"request_id"`
	Snapshot   snapshotResponse   `json:"snapshot"`
	Connection connectionResponse `json:"connection"`
}

type createSnapshotRequest struct {
	Revision      int64  `json:"revision"`
	WindowStart   string `json:"window_start"`
	WindowEnd     string `json:"window_end"`
	ExpectedPages int    `json:"expected_pages"`
}

// createSnapshot은 snapshot session을 만든다. 연결은 읽기 source가 하나 이상 선택돼 있어야 하고
// 요청 기기가 sync 기기(또는 아직 없으면 이 기기가 지정된다)여야 한다. 이전에 열린 session이 있으면
// abort한다. 같은 revision을 같은 값으로 다시 보내면 같은 session을 돌려준다.
func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.enforce(w, r, ratelimit.ScopeCalendarSnapshot, ratelimit.LimitCalendarSnapshot, p) {
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	var req createSnapshotRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	ws, we, err := parseWindow(req.WindowStart, req.WindowEnd)
	if err != nil || req.Revision <= 0 || req.ExpectedPages < 0 || req.ExpectedPages > maxPages {
		h.writeDomainError(w, r, "calendar.snapshot.create", errInvalidInput)
		return
	}

	out, ok := h.run(w, r, p, prep, "calendar.snapshot.create", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		var (
			connID  uuid.UUID
			status  string
			syncDev *uuid.UUID
			lastRev int64
			version int64
		)
		err := tx.QueryRow(ctx, `
			SELECT id, source_status, sync_device_id, last_snapshot_revision, version
			  FROM calendar_connections WHERE user_id = $1 FOR UPDATE`, p.UserID).Scan(&connID, &status, &syncDev, &lastRev, &version)
		if isNoRows(err) {
			return mutation.Result{}, errStateInvalid
		}
		if err != nil {
			return mutation.Result{}, err
		}
		if syncDev != nil && *syncDev != p.DeviceID {
			return mutation.Result{}, errForbidden
		}
		switch status {
		case "selecting", "syncing", "ready", "error":
		default:
			return mutation.Result{}, errStateInvalid
		}
		var enabled int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM calendar_source_selections WHERE connection_id = $1 AND enabled`, connID).Scan(&enabled); err != nil {
			return mutation.Result{}, err
		}
		if enabled == 0 {
			return mutation.Result{}, errSourceRequired
		}

		// 같은 revision: 값이 같으면 같은 session(열려 있으면 이어서 업로드, 끝났으면 결과 재사용).
		var (
			existing   uuid.UUID
			exWS, exWE time.Time
			exPages    int
		)
		err = tx.QueryRow(ctx, `
			SELECT id, window_start, window_end, expected_pages FROM calendar_snapshot_sessions
			 WHERE connection_id = $1 AND revision = $2`, connID, req.Revision).Scan(&existing, &exWS, &exWE, &exPages)
		if err == nil {
			if !exWS.Equal(ws) || !exWE.Equal(we) || exPages != req.ExpectedPages {
				return mutation.Result{}, &mutation.VersionConflictError{Current: lastRev}
			}
			return mutation.Result{StatusCode: http.StatusOK, ResourceType: "calendar_snapshot", ResourceID: existing, ResourceVersion: 1}, nil
		}
		if !isNoRows(err) {
			return mutation.Result{}, err
		}
		// 새 revision은 지금까지 받아들인 revision보다 커야 한다. 지난 revision의 늦은 업로드가 최신
		// 상태를 되돌리지 못하게 한다. current_version에 마지막 revision을 실어 클라이언트가 이어간다.
		if req.Revision <= lastRev {
			return mutation.Result{}, &mutation.VersionConflictError{Current: lastRev}
		}

		// 열린 session은 하나뿐이다. 이전 것은 abort하고 staging을 지운다.
		if _, err := tx.Exec(ctx, `
			DELETE FROM calendar_snapshot_facts_staging
			 WHERE session_id IN (SELECT id FROM calendar_snapshot_sessions WHERE connection_id = $1 AND status = 'open')`, connID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM calendar_snapshot_pages
			 WHERE session_id IN (SELECT id FROM calendar_snapshot_sessions WHERE connection_id = $1 AND status = 'open')`, connID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE calendar_snapshot_sessions SET status = 'aborted' WHERE connection_id = $1 AND status = 'open'`, connID); err != nil {
			return mutation.Result{}, err
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO calendar_snapshot_sessions (id, connection_id, revision, window_start, window_end, expected_pages)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, connID, req.Revision, ws, we, req.ExpectedPages); err != nil {
			return mutation.Result{}, err
		}
		// 첫 snapshot이거나 기기를 이 기기로 정한다. 상태는 syncing이지만 마지막 정상 데이터는 유지한다.
		if _, err := tx.Exec(ctx, `
			UPDATE calendar_connections
			   SET sync_device_id = $2, source_status = CASE WHEN source_status = 'selecting' OR source_status = 'error' OR source_status = 'ready' THEN 'syncing' ELSE source_status END,
			       last_snapshot_revision = $3, version = version + 1, updated_at = now()
			 WHERE id = $1`, connID, p.DeviceID, req.Revision); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusCreated, ResourceType: "calendar_snapshot", ResourceID: id, ResourceVersion: 1}, nil
	})
	if !ok {
		return
	}
	h.writeSnapshot(w, r, p, out.ResourceID, out.StatusCode)
}

func (h *Handler) writeSnapshot(w http.ResponseWriter, r *http.Request, p auth.Principal, sessionID uuid.UUID, status int) {
	var s snapshotResponse
	var ws, we time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT s.id, s.revision, s.window_start, s.window_end, s.expected_pages, s.status, s.fact_count, s.generation
		  FROM calendar_snapshot_sessions s
		  JOIN calendar_connections c ON c.id = s.connection_id
		 WHERE s.id = $1 AND c.user_id = $2`, sessionID, p.UserID).Scan(
		&s.ID, &s.Revision, &ws, &we, &s.ExpectedPages, &s.Status, &s.FactCount, &s.Generation)
	if isNoRows(err) {
		h.writeDomainError(w, r, "calendar.snapshot.get", errNotFound)
		return
	}
	if err != nil {
		h.writeDomainError(w, r, "calendar.snapshot.get", err)
		return
	}
	s.WindowStart, s.WindowEnd = ws.UTC().Format(time.RFC3339), we.UTC().Format(time.RFC3339)
	c, err := h.loadConnection(r.Context(), h.pool, p.UserID, p.DeviceID)
	if err != nil {
		h.writeDomainError(w, r, "calendar.snapshot.get", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, status, snapshotEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Snapshot: s, Connection: c})
}

func parseWindow(start, end string) (time.Time, time.Time, error) {
	ws, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return time.Time{}, time.Time{}, errInvalidInput
	}
	we, err := time.Parse(time.RFC3339, end)
	if err != nil {
		return time.Time{}, time.Time{}, errInvalidInput
	}
	if !we.After(ws) || we.Sub(ws) > maxWindow {
		return time.Time{}, time.Time{}, errInvalidInput
	}
	return ws.UTC(), we.UTC(), nil
}

type factJSON struct {
	SourceEventKey string `json:"source_event_key"`
	StartAt        string `json:"start_at"`
	EndAt          string `json:"end_at"`
	AllDay         bool   `json:"all_day"`
	TimeZone       string `json:"time_zone"`
	Availability   string `json:"availability"`
}

type pageRequest struct {
	Facts []factJSON `json:"facts"`
}

type parsedFact struct {
	key          []byte
	start, end   time.Time
	allDay       bool
	timeZone     string
	availability string
}

func parseFact(f factJSON, ws, we time.Time) (parsedFact, error) {
	key, err := base64.RawURLEncoding.DecodeString(f.SourceEventKey)
	if err != nil || len(key) < minKeyBytes || len(key) > maxKeyBytes {
		return parsedFact{}, errInvalidInput
	}
	start, err := time.Parse(time.RFC3339, f.StartAt)
	if err != nil {
		return parsedFact{}, errInvalidInput
	}
	end, err := time.Parse(time.RFC3339, f.EndAt)
	if err != nil {
		return parsedFact{}, errInvalidInput
	}
	start, end = start.UTC(), end.UTC()
	// window와 겹치는 일정만 받는다. 경계를 가로지르는 일정은 포함한다(설계 3 §6.1).
	if !end.After(start) || end.Sub(start) > maxFactDuration || !end.After(ws) || !start.Before(we) {
		return parsedFact{}, errInvalidInput
	}
	if len(f.TimeZone) == 0 || len(f.TimeZone) > 64 {
		return parsedFact{}, errInvalidInput
	}
	if !validTimeZone(f.TimeZone) {
		return parsedFact{}, errInvalidInput
	}
	// free는 계산에서 제외되므로 기기가 올리지 않는다(설계 3 §6.2).
	switch f.Availability {
	case "busy", "tentative", "unavailable":
	default:
		return parsedFact{}, errInvalidInput
	}
	return parsedFact{key: key, start: start, end: end, allDay: f.AllDay, timeZone: f.TimeZone, availability: f.Availability}, nil
}

// putPage는 page 하나를 통째로 교체한다. 같은 page를 다시 보내면 같은 결과다(재시도 안전).
func (h *Handler) putPage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.enforce(w, r, ratelimit.ScopeCalendarPage, ratelimit.LimitCalendarPage, p) {
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.writeDomainError(w, r, "calendar.snapshot.page", errNotFound)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= maxPages {
		h.writeDomainError(w, r, "calendar.snapshot.page", errInvalidInput)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문을 읽을 수 없다")
		return
	}
	var req pageRequest
	if err := httpapi.DecodeJSONBytes(body, &req); err != nil || len(req.Facts) > maxFactsPerPage {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}

	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		var (
			connID  uuid.UUID
			syncDev *uuid.UUID
			status  string
			pages   int
			ws, we  time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT c.id, c.sync_device_id, s.status, s.expected_pages, s.window_start, s.window_end
			  FROM calendar_snapshot_sessions s
			  JOIN calendar_connections c ON c.id = s.connection_id
			 WHERE s.id = $1 AND c.user_id = $2
			   FOR UPDATE OF s`, sessionID, p.UserID).Scan(&connID, &syncDev, &status, &pages, &ws, &we)
		if isNoRows(err) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if syncDev == nil || *syncDev != p.DeviceID {
			return errForbidden
		}
		if status != "open" {
			return errSnapshotState
		}
		if n >= pages {
			return errInvalidInput
		}
		facts := make([]parsedFact, 0, len(req.Facts))
		seen := make(map[string]struct{}, len(req.Facts))
		for _, f := range req.Facts {
			pf, err := parseFact(f, ws, we)
			if err != nil {
				return err
			}
			if _, dup := seen[string(pf.key)]; dup {
				return errInvalidInput
			}
			seen[string(pf.key)] = struct{}{}
			facts = append(facts, pf)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_facts_staging WHERE session_id = $1 AND page = $2`, sessionID, n); err != nil {
			return err
		}
		for _, f := range facts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO calendar_snapshot_facts_staging
				       (session_id, page, source_event_key, start_at, end_at, all_day, time_zone, availability)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				sessionID, n, f.key, f.start, f.end, f.allDay, f.timeZone, f.availability); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					// 다른 page와 source_event_key가 겹친다.
					return errInvalidInput
				}
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO calendar_snapshot_pages (session_id, page, fact_count) VALUES ($1, $2, $3)
			ON CONFLICT (session_id, page) DO UPDATE SET fact_count = EXCLUDED.fact_count`, sessionID, n, len(facts))
		return err
	})
	if err != nil {
		h.writeDomainError(w, r, "calendar.snapshot.page", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"request_id": httpapi.RequestID(r.Context()).String(), "page": n, "fact_count": len(req.Facts),
	})
}

type completeRequest struct {
	FactCount int `json:"fact_count"`
}

// completeSnapshot은 검증 후 facts를 새 generation으로 원자적으로 교체한다. 같은 session을 다시
// complete하면 같은 결과다. 실패하면 기존 generation과 projection은 그대로다.
func (h *Handler) completeSnapshot(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.writeDomainError(w, r, "calendar.snapshot.complete", errNotFound)
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	var req completeRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil || req.FactCount < 0 || req.FactCount > maxTotalFacts {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}

	out, ok := h.run(w, r, p, prep, "calendar.snapshot.complete", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		// 잠금 순서: 사용자의 활성 Party(FOR SHARE) -> connection(FOR UPDATE) -> 멤버십(KEY SHARE).
		// Party 행을 먼저 잡아야 같은 Party에 가입하는 T3와 직렬화된다.
		if err := party.LockUserPartiesShared(ctx, tx, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		var connID uuid.UUID
		var syncDev *uuid.UUID
		var gen int64
		var connStatus string
		err := tx.QueryRow(ctx, `
			SELECT c.id, c.sync_device_id, c.active_generation, c.source_status
			  FROM calendar_connections c
			  JOIN calendar_snapshot_sessions s ON s.connection_id = c.id
			 WHERE s.id = $1 AND c.user_id = $2
			   FOR UPDATE OF c`, sessionID, p.UserID).Scan(&connID, &syncDev, &gen, &connStatus)
		if isNoRows(err) {
			return mutation.Result{}, errNotFound
		}
		if err != nil {
			return mutation.Result{}, err
		}
		if syncDev == nil || *syncDev != p.DeviceID {
			return mutation.Result{}, errForbidden
		}
		var status string
		var expected int
		if err := tx.QueryRow(ctx, `SELECT status, expected_pages FROM calendar_snapshot_sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&status, &expected); err != nil {
			return mutation.Result{}, err
		}
		switch status {
		case "completed":
			// 같은 revision의 complete 재호출은 같은 결과다(설계 3 §7.2).
			return mutation.Result{StatusCode: http.StatusOK, ResourceType: "calendar_snapshot", ResourceID: sessionID, ResourceVersion: 1}, nil
		case "open":
		default:
			return mutation.Result{}, errSnapshotState
		}
		// syncing이 아닌 연결은 완료할 수 없다. 업로드 도중 사용자가 권한을 철회했거나(revoked 등)
		// 연결을 끊었으면 늦게 도착한 complete가 연결을 ready로 되살려 projection을 다시 노출하면
		// 안 된다(설계 3 §12). PUT이 열린 session을 abort하지만 이 확인이 마지막 방어선이다.
		if connStatus != "syncing" {
			return mutation.Result{}, errStateInvalid
		}

		var uploaded, total int
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(sum(fact_count), 0) FROM calendar_snapshot_pages WHERE session_id = $1`, sessionID).Scan(&uploaded, &total); err != nil {
			return mutation.Result{}, err
		}
		var staged int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM calendar_snapshot_facts_staging WHERE session_id = $1`, sessionID).Scan(&staged); err != nil {
			return mutation.Result{}, err
		}
		// 모든 page가 올라왔고 개수가 클라이언트 선언·page 합계와 모두 같아야 한다.
		if uploaded != expected || total != req.FactCount || staged != req.FactCount || staged > maxTotalFacts {
			return mutation.Result{}, errSnapshotIncomplete
		}

		newGen := gen + 1
		if _, err := tx.Exec(ctx, `DELETE FROM calendar_busy_facts WHERE connection_id = $1`, connID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO calendar_busy_facts
			       (connection_id, source_event_key, user_id, generation, start_at, end_at, all_day, time_zone, availability)
			SELECT $1, source_event_key, $2, $3, start_at, end_at, all_day, time_zone, availability
			  FROM calendar_snapshot_facts_staging WHERE session_id = $4`, connID, p.UserID, newGen, sessionID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE calendar_connections
			   SET source_status = 'ready', active_generation = $2, last_completed_sync_at = now(),
			       revoked_at = NULL, version = version + 1, updated_at = now()
			 WHERE id = $1`, connID, newGen); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE calendar_snapshot_sessions
			   SET status = 'completed', generation = $2, fact_count = $3, completed_at = now()
			 WHERE id = $1`, sessionID, newGen, req.FactCount); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_facts_staging WHERE session_id = $1`, sessionID); err != nil {
			return mutation.Result{}, err
		}
		// 사용자가 속한 모든 Party의 busy projection을 새 generation에 맞춘다. connection은 이미
		// FOR UPDATE로 잡고 있다.
		if err := party.RebuildBusyProjections(ctx, tx, p.UserID, nil, true); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: "calendar_snapshot", ResourceID: sessionID, ResourceVersion: newGen}, nil
	})
	if !ok {
		return
	}
	h.writeSnapshot(w, r, p, out.ResourceID, out.StatusCode)
}

var errSnapshotIncomplete = errors.New("calendar: snapshot이 아직 완전하지 않다")

// abortSnapshot은 열린 session을 폐기한다. 기존 generation과 projection에는 영향이 없다.
func (h *Handler) abortSnapshot(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.writeDomainError(w, r, "calendar.snapshot.abort", errNotFound)
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	if len(bytes.TrimSpace(prep.Body)) != 0 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "본문을 보내지 않는다")
		return
	}
	out, ok := h.run(w, r, p, prep, "calendar.snapshot.abort", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		var connID uuid.UUID
		var syncDev *uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT c.id, c.sync_device_id FROM calendar_connections c
			  JOIN calendar_snapshot_sessions s ON s.connection_id = c.id
			 WHERE s.id = $1 AND c.user_id = $2
			   FOR UPDATE OF c`, sessionID, p.UserID).Scan(&connID, &syncDev)
		if isNoRows(err) {
			return mutation.Result{}, errNotFound
		}
		if err != nil {
			return mutation.Result{}, err
		}
		if syncDev == nil || *syncDev != p.DeviceID {
			return mutation.Result{}, errForbidden
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM calendar_snapshot_sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&status); err != nil {
			return mutation.Result{}, err
		}
		if status == "completed" {
			return mutation.Result{}, errSnapshotState
		}
		if status == "open" {
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_facts_staging WHERE session_id = $1`, sessionID); err != nil {
				return mutation.Result{}, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_snapshot_pages WHERE session_id = $1`, sessionID); err != nil {
				return mutation.Result{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE calendar_snapshot_sessions SET status = 'aborted' WHERE id = $1`, sessionID); err != nil {
				return mutation.Result{}, err
			}
			// syncing이던 연결은 마지막 정상 generation이 있으면 ready, 없으면 selecting으로 돌린다.
			if _, err := tx.Exec(ctx, `
				UPDATE calendar_connections
				   SET source_status = CASE WHEN active_generation > 0 AND last_completed_sync_at IS NOT NULL THEN 'ready' ELSE 'selecting' END,
				       version = version + 1, updated_at = now()
				 WHERE id = $1 AND source_status = 'syncing'`, connID); err != nil {
				return mutation.Result{}, err
			}
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: "calendar_snapshot", ResourceID: sessionID, ResourceVersion: 1}, nil
	})
	if !ok {
		return
	}
	h.writeSnapshot(w, r, p, out.ResourceID, out.StatusCode)
}
