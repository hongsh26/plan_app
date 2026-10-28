package party

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
)

type scheduleItem struct {
	ID          uuid.UUID `json:"id"`
	OwnerUserID uuid.UUID `json:"ownerUserId"`
	StartAt     string    `json:"startAt"`
	EndAt       string    `json:"endAt"`
	AllDay      bool      `json:"allDay"`
	TimeZone    string    `json:"timeZone"`
	DisplayKind string    `json:"displayKind"`
	Title       *string   `json:"title"`
	Location    *string   `json:"location"`
	Version     int64     `json:"version"`
}

type scheduleEnvelope struct {
	RequestID   string         `json:"request_id"`
	Projections []scheduleItem `json:"projections"`
}

func (h *Handler) registerSchedule(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/parties/{id}/schedule", require(http.HandlerFunc(h.getSchedule)))
}

func (h *Handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	start, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("start_at"))
	end, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("end_at"))
	if err1 != nil || err2 != nil || !end.After(start) || end.Sub(start) > 31*24*time.Hour {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "조회 범위가 올바르지 않다")
		return
	}
	if err := requireVisibilityMember(r.Context(), h.pool, partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.schedule.get", err)
		return
	}
	items, err := h.loadSchedule(r.Context(), partyID, p.UserID, start, end)
	if err != nil {
		h.writeDomainError(w, r, "party.schedule.get", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, http.StatusOK, scheduleEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Projections: items})
}

func (h *Handler) loadSchedule(ctx context.Context, partyID, viewerID uuid.UUID, start, end time.Time) ([]scheduleItem, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT q.id, q.owner_user_id, q.start_at, q.end_at, q.all_day, q.time_zone,
		       q.visibility_level, q.title_ciphertext, q.location_ciphertext, q.version
		  FROM party_schedule_projections q
		 WHERE q.party_id = $1 AND q.start_at < $3 AND q.end_at > $2
		   AND EXISTS (SELECT 1 FROM party_memberships m
		                JOIN parties p ON p.id = m.party_id
		                WHERE m.party_id = q.party_id AND m.user_id = $4
		                  AND m.status = 'active' AND p.status = 'active')
		 ORDER BY q.start_at, q.id`, partyID, start.UTC(), end.UTC(), viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]scheduleItem, 0)
	for rows.Next() {
		var item scheduleItem
		var startAt, endAt time.Time
		var titleBox, locationBox []byte
		if err := rows.Scan(&item.ID, &item.OwnerUserID, &startAt, &endAt, &item.AllDay,
			&item.TimeZone, &item.DisplayKind, &titleBox, &locationBox, &item.Version); err != nil {
			return nil, err
		}
		item.StartAt, item.EndAt = startAt.UTC().Format(time.RFC3339), endAt.UTC().Format(time.RFC3339)
		if item.DisplayKind == visibilityDetails {
			if h.projectionOpener == nil {
				item.DisplayKind = visibilityBusyOnly
			} else if titleBox == nil {
				untitled := "제목 없는 일정"
				item.Title = &untitled
			} else {
				title, err := h.projectionOpener.Open(titleBox, "party_projection_title:"+item.ID.String())
				if err != nil {
					h.scheduleDecryptFailure(ctx)
					item.DisplayKind = visibilityBusyOnly
				} else {
					value := string(title)
					item.Title = &value
				}
			}
			if item.DisplayKind == visibilityDetails && locationBox != nil {
				location, err := h.projectionOpener.Open(locationBox, "party_projection_location:"+item.ID.String())
				if err != nil {
					h.scheduleDecryptFailure(ctx)
					item.DisplayKind, item.Title = visibilityBusyOnly, nil
				} else {
					value := string(location)
					item.Location = &value
				}
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (h *Handler) scheduleDecryptFailure(ctx context.Context) {
	if h.logger != nil {
		h.logger.ErrorContext(ctx, "Party 일정 상세 복호화 실패", slog.String("action", "party.schedule.get"), slog.String("result", "decrypt_failed"))
	}
}
