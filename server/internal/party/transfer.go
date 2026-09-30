package party

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
)

// jobOwnerTransferred는 설계 §9.3의 알림 job이다. dedupe key는 party_id:party_version이다.
const jobOwnerTransferred = "notify_owner_transferred"

// errInvalidTransferTarget은 위임 대상이 같은 Party의 활성 멤버가 아닐 때다(400).
var errInvalidTransferTarget = errors.New("party: 위임 대상이 활성 멤버가 아니다")

func (h *Handler) registerOwnerTransfer(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/parties/{id}/owner", require(http.HandlerFunc(h.transferOwner)))
}

type transferOwnerRequest struct {
	UserID string `json:"user_id"`
}

// transferOwner는 설계 §5.5 T4다. 요청자가 방장이고 If-Match가 parties.version과 같을 때
// 같은 Party의 다른 활성 멤버에게 방장을 넘긴다. 성공 응답은 요청자가 더 이상 방장이 아닌
// 새 Party 상태다.
//
// 잠금 순서는 parties -> party_memberships(id 순) -> party_invites다(§5.5). 초대 수락(T3)은
// users -> parties -> party_invites라 서로 교착하지 않는다.
func (h *Handler) transferOwner(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	var req transferOwnerRequest
	if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	targetUserID, err := uuid.Parse(req.UserID)
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "user_id가 올바르지 않다")
		return
	}
	if targetUserID == p.UserID {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "자기 자신에게 위임할 수 없다")
		return
	}
	// 재전송에서도 현재 권한을 먼저 평가한다(§9.4). 이미 위임한 요청자는 403이다.
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.transfer_owner", err)
		return
	}

	out, ok := h.run(w, r, p, prep, "party.transfer_owner", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		current, err := lockPartyForMutation(ctx, tx, partyID, p.UserID)
		if err != nil {
			return mutation.Result{}, err
		}
		if err := requireOwner(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := mutation.CheckVersion(prep.ExpectedVersion, current); err != nil {
			return mutation.Result{}, err
		}

		// 두 멤버십을 id 순으로 잠근다. 잠근 뒤에 역할과 상태를 다시 읽어 판정한다.
		rows, err := tx.Query(ctx, `
			SELECT m.id, m.user_id, m.role
			  FROM party_memberships m
			 WHERE m.party_id = $1 AND m.status = 'active' AND m.user_id IN ($2, $3)
			 ORDER BY m.id
			   FOR UPDATE`, partyID, p.UserID, targetUserID)
		if err != nil {
			return mutation.Result{}, err
		}
		var oldOwnerMembership, newOwnerMembership uuid.UUID
		for rows.Next() {
			var id, userID uuid.UUID
			var role string
			if err := rows.Scan(&id, &userID, &role); err != nil {
				rows.Close()
				return mutation.Result{}, err
			}
			switch {
			case userID == p.UserID && role == "owner":
				oldOwnerMembership = id
			case userID == targetUserID:
				newOwnerMembership = id
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mutation.Result{}, err
		}
		if oldOwnerMembership == uuid.Nil {
			return mutation.Result{}, errForbidden
		}
		if newOwnerMembership == uuid.Nil {
			// 설계 §10: 활성 멤버가 아닌 대상은 400이다. 호출자가 방장이므로 존재를 숨길 이유가 없다.
			return mutation.Result{}, errInvalidTransferTarget
		}

		// owner partial unique index는 즉시 검증되므로 기존 방장을 먼저 내리고 대상을 올린다.
		if _, err := tx.Exec(ctx, `
			UPDATE party_memberships
			   SET role = 'member', version = version + 1, updated_at = now()
			 WHERE id = $1`, oldOwnerMembership); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE party_memberships
			   SET role = 'owner', version = version + 1, updated_at = now()
			 WHERE id = $1`, newOwnerMembership); err != nil {
			return mutation.Result{}, err
		}
		var version int64
		if err := tx.QueryRow(ctx, `
			UPDATE parties
			   SET owner_membership_id = $2, version = version + 1, updated_at = now()
			 WHERE id = $1
			RETURNING version`, partyID, newOwnerMembership).Scan(&version); err != nil {
			return mutation.Result{}, err
		}

		// 이전 방장이 뿌린 링크가 새 방장 모르게 멤버를 늘리지 못하게 활성 초대를 모두 무효화한다.
		revoked, err := revokeActiveInvites(ctx, tx, partyID, h.now())
		if err != nil {
			return mutation.Result{}, err
		}

		members, err := activeMemberships(ctx, tx, partyID)
		if err != nil {
			return mutation.Result{}, err
		}
		for _, m := range members {
			for _, id := range []uuid.UUID{partyID, oldOwnerMembership, newOwnerMembership} {
				if err := emitProjection(ctx, tx, m.userID, id); err != nil {
					return mutation.Result{}, err
				}
			}
		}
		// 초대 entity는 방장만 본다(§6, §9.2). 무효화 tombstone은 이전 방장과 새 방장에게만 간다.
		for _, inviteID := range revoked {
			for _, recipient := range []uuid.UUID{p.UserID, targetUserID} {
				if err := tx.EmitChange(ctx, mutation.Change{Recipient: recipient, EntityType: entityPartyInvite, EntityID: inviteID}); err != nil {
					return mutation.Result{}, err
				}
			}
		}

		dedupe := partyID.String() + ":" + strconv.FormatInt(version, 10)
		if err := tx.Enqueue(ctx, mutation.Job{
			Type:      jobOwnerTransferred,
			Payload:   map[string]string{"party_id": partyID.String(), "membership_id": newOwnerMembership.String()},
			DedupeKey: &dedupe,
		}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityParty, ResourceID: partyID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	// 응답은 위임 직후의 Party다. 요청자는 이제 일반 멤버이므로 멤버 기준으로 읽는다.
	party, err := loadPartyForMember(r.Context(), h.pool, partyID, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if err != nil {
		h.writeDomainError(w, r, "party.transfer_owner", err)
		return
	}
	writePartyResponse(w, r, party, out.StatusCode)
}

// revokeActiveInvites는 Party의 active 초대를 모두 revoked로 바꾸고 그 ID를 돌려준다.
// 만료 시각이 지났어도 저장된 status가 active면 함께 바꾼다. 행을 id 순으로 잠가 다른
// 트랜잭션과 잠금 순서가 어긋나지 않게 한다.
func revokeActiveInvites(ctx context.Context, tx *mutation.Tx, partyID uuid.UUID, now time.Time) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		WITH locked AS (
			SELECT id FROM party_invites
			 WHERE party_id = $1 AND status = 'active'
			 ORDER BY id
			   FOR UPDATE
		)
		UPDATE party_invites i
		   SET status = 'revoked', revoked_at = $2, updated_at = $2
		  FROM locked
		 WHERE i.id = locked.id
		RETURNING i.id`, partyID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
