package party

// P3 초대. party_membership_design.md §4.2, §5.4, §5.5 T2·T3, §9.1.
//
// token 원문은 생성 응답 본문에만 존재한다. DB에는 SHA-256만, 로그·audit·sync·outbox에는
// 아무것도 남기지 않는다(§7.4). 멱등성 기록(mutation)은 응답 본문이 아니라 결과 포인터만
// 저장하므로(§8) 같은 Idempotency-Key로 재전송하면 초대 메타데이터는 돌려주지만 token
// 원문은 다시 줄 수 없다. 원문을 저장하지 않는 것이 §4.2의 요구이므로, 클라이언트가 응답을
// 잃었다면 초대를 무효화하고 새로 만든다.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/mutation"
	"plantogether/server/internal/platform/ratelimit"
)

const (
	codeAlreadyMember           = "already_member"
	codePartyFull               = "party_full"
	codePartyInviteLimitReached = "party_invite_limit_reached"

	jobMemberJoined = "notify_member_joined"

	defaultInviteTTL      = 7 * 24 * time.Hour
	minInviteTTL          = time.Hour
	maxInviteTTL          = 30 * 24 * time.Hour
	maxInviteUses         = 10
	maxActiveInvites      = 3
	maxInvitesPerHour     = 10
	maxTokenLength        = 128
	inviteRateLimitWindow = time.Hour
)

var (
	errAlreadyMember    = errors.New("party: 이미 활성 멤버다")
	errPartyFull        = errors.New("party: 정원이 찼다")
	errInviteLimit      = errors.New("party: 활성 초대 한도에 도달했다")
	errInviteRateLimits = errors.New("party: 초대 생성 제한을 넘었다")
)

func (h *Handler) registerInvites(mux *http.ServeMux, require func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/parties/{id}/invites", require(http.HandlerFunc(h.createInvite)))
	mux.Handle("GET /v1/parties/{id}/invites", require(http.HandlerFunc(h.listInvites)))
	mux.Handle("DELETE /v1/parties/{id}/invites/{invite_id}", require(http.HandlerFunc(h.revokeInvite)))
	mux.Handle("GET /v1/invites/{token}/preview", require(http.HandlerFunc(h.previewInvite)))
	mux.Handle("POST /v1/invites/{token}/accept", require(http.HandlerFunc(h.acceptInvite)))
}

type inviteResponse struct {
	ID        uuid.UUID `json:"id"`
	PartyID   uuid.UUID `json:"party_id"`
	Status    string    `json:"status"`
	ExpiresAt string    `json:"expires_at"`
	MaxUses   int16     `json:"max_uses"`
	UsedCount int16     `json:"used_count"`
	CreatedAt string    `json:"created_at"`
	// Token은 생성 응답에만 채워진다. 목록·재전송·무효화 응답에는 없다.
	Token string `json:"token,omitempty"`
}

type inviteEnvelope struct {
	RequestID string         `json:"request_id"`
	Invite    inviteResponse `json:"invite"`
}

type invitesEnvelope struct {
	RequestID string           `json:"request_id"`
	Invites   []inviteResponse `json:"invites"`
}

type createInviteRequest struct {
	ExpiresInSeconds *int64 `json:"expires_in_seconds"`
	MaxUses          *int   `json:"max_uses"`
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	var req createInviteRequest
	if len(strings.TrimSpace(string(prep.Body))) > 0 {
		if err := httpapi.DecodeJSONBytes(prep.Body, &req); err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
			return
		}
	}
	ttl := defaultInviteTTL
	if req.ExpiresInSeconds != nil {
		ttl = time.Duration(*req.ExpiresInSeconds) * time.Second
		if *req.ExpiresInSeconds < int64(minInviteTTL/time.Second) || *req.ExpiresInSeconds > int64(maxInviteTTL/time.Second) {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "만료 시간은 1시간~30일이어야 한다")
			return
		}
	}
	if req.MaxUses != nil && (*req.MaxUses < 1 || *req.MaxUses > maxInviteUses) {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "max_uses는 1~10이어야 한다")
		return
	}
	// 재전송에서도 현재 권한을 먼저 평가한다(§9.4).
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.invite.create", err)
		return
	}

	now := h.now()
	var token string
	out, ok := h.run(w, r, p, prep, "party.invite.create", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		if _, err := lockPartyForMutation(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := requireOwner(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		var creatorMembershipID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT id FROM party_memberships
			 WHERE party_id = $1 AND user_id = $2 AND status = 'active'`, partyID, p.UserID).Scan(&creatorMembershipID); err != nil {
			return mutation.Result{}, err
		}

		var recent, active int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM party_invites
			 WHERE party_id = $1 AND created_at > $2`, partyID, now.Add(-inviteRateLimitWindow)).Scan(&recent); err != nil {
			return mutation.Result{}, err
		}
		if recent >= maxInvitesPerHour {
			return mutation.Result{}, errInviteRateLimits
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM party_invites
			 WHERE party_id = $1 AND status = 'active' AND expires_at > $2`, partyID, now).Scan(&active); err != nil {
			return mutation.Result{}, err
		}
		if active >= maxActiveInvites {
			return mutation.Result{}, errInviteLimit
		}

		var members, limit int
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM party_memberships WHERE party_id = p.id AND status = 'active'),
			       p.member_limit
			  FROM parties p WHERE p.id = $1`, partyID).Scan(&members, &limit); err != nil {
			return mutation.Result{}, err
		}
		remaining := limit - members
		if remaining <= 0 {
			return mutation.Result{}, errPartyFull
		}
		maxUses := max(1, min(remaining, maxInviteUses))
		if req.MaxUses != nil {
			maxUses = *req.MaxUses
		}

		t, err := newToken()
		if err != nil {
			return mutation.Result{}, err
		}
		inviteID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_invites (id, party_id, token_hash, expires_at, max_uses, created_by_membership_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			inviteID, partyID, hashToken(t), now.Add(ttl), maxUses, creatorMembershipID, now); err != nil {
			return mutation.Result{}, err
		}
		if err := emitInviteUpsert(ctx, tx, p.UserID, inviteID); err != nil {
			return mutation.Result{}, err
		}
		token = t
		return mutation.Result{StatusCode: http.StatusCreated, ResourceType: entityPartyInvite, ResourceID: inviteID, ResourceVersion: 1}, nil
	})
	if !ok {
		return
	}
	inv, err := loadInvite(r.Context(), h.pool, partyID, out.ResourceID, now)
	if err != nil {
		h.writeInviteLoadError(w, r, "party.invite.create", err)
		return
	}
	if !out.Replayed {
		inv.Token = token
	}
	httpapi.WriteJSON(w, out.StatusCode, inviteEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Invite: inv})
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.invite.list", err)
		return
	}
	now := h.now()
	rows, err := h.pool.Query(r.Context(), `
		SELECT id, party_id, status, expires_at, max_uses, used_count, created_at
		  FROM party_invites
		 WHERE party_id = $1 AND status = 'active' AND expires_at > $2
		 ORDER BY created_at, id`, partyID, now)
	if err != nil {
		h.writeDomainError(w, r, "party.invite.list", err)
		return
	}
	defer rows.Close()
	out := invitesEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Invites: []inviteResponse{}}
	for rows.Next() {
		inv, err := scanInvite(rows, now)
		if err != nil {
			h.writeDomainError(w, r, "party.invite.list", err)
			return
		}
		out.Invites = append(out.Invites, inv)
	}
	if err := rows.Err(); err != nil {
		h.writeDomainError(w, r, "party.invite.list", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	partyID, ok := parsePartyID(w, r)
	if !ok {
		return
	}
	inviteID, err := uuid.Parse(r.PathValue("invite_id"))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	if err := h.requireCurrentOwner(r.Context(), partyID, p.UserID); err != nil {
		h.writeDomainError(w, r, "party.invite.revoke", err)
		return
	}
	now := h.now()
	out, ok := h.run(w, r, p, prep, "party.invite.revoke", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		if _, err := lockPartyForMutation(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := requireOwner(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		var status string
		var used int16
		err := tx.QueryRow(ctx, `
			SELECT status, used_count FROM party_invites
			 WHERE id = $1 AND party_id = $2 FOR UPDATE`, inviteID, partyID).Scan(&status, &used)
		if errors.Is(err, pgx.ErrNoRows) {
			return mutation.Result{}, errNotFound
		}
		if err != nil {
			return mutation.Result{}, err
		}
		// 활성이 아니면 이미 끝난 초대다. 상태를 되돌리거나 덮어쓰지 않고 현재 값을 돌려준다.
		if status == "active" {
			if _, err := tx.Exec(ctx, `
				UPDATE party_invites
				   SET status = 'revoked', revoked_at = $2, updated_at = $2
				 WHERE id = $1`, inviteID, now); err != nil {
				return mutation.Result{}, err
			}
			if err := tx.EmitChange(ctx, mutation.Change{Recipient: p.UserID, EntityType: entityPartyInvite, EntityID: inviteID}); err != nil {
				return mutation.Result{}, err
			}
		}
		return mutation.Result{StatusCode: http.StatusOK, ResourceType: entityPartyInvite, ResourceID: inviteID, ResourceVersion: int64(used) + 1}, nil
	})
	if !ok {
		return
	}
	inv, err := loadInvite(r.Context(), h.pool, partyID, out.ResourceID, now)
	if err != nil {
		h.writeInviteLoadError(w, r, "party.invite.revoke", err)
		return
	}
	httpapi.WriteJSON(w, out.StatusCode, inviteEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Invite: inv})
}

type invitePreview struct {
	PartyName          string `json:"party_name"`
	MemberCount        int    `json:"member_count"`
	InviterDisplayName string `json:"inviter_display_name"`
}

type previewEnvelope struct {
	RequestID string        `json:"request_id"`
	Preview   invitePreview `json:"preview"`
}

// previewInvite는 §7.2 필드만 돌려준다. 만료·무효·소진·해산·미존재는 모두 같은 404다.
func (h *Handler) previewInvite(w http.ResponseWriter, r *http.Request) {
	// 요청 제한은 token 길이 검사와 조회보다 먼저다. 잘못된 token이 404로 끝나는 시도도
	// 모두 센다(docs/rate_limit_design.md §4.2). 그렇지 않으면 미리보기가 token 유효성을
	// 알려주는 검증 경로가 된다(§4.2).
	if !h.enforceInviteLimit(w, r, ratelimit.ScopeInvitePreview, ratelimit.LimitPreviewUser, ratelimit.LimitPreviewIP) {
		return
	}
	token := r.PathValue("token")
	if token == "" || len(token) > maxTokenLength {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	var pv invitePreview
	err := h.pool.QueryRow(r.Context(), `
		SELECT p.name,
		       (SELECT count(*) FROM party_memberships WHERE party_id = p.id AND status = 'active'),
		       u.display_name
		  FROM party_invites i
		  JOIN parties p ON p.id = i.party_id
		  JOIN party_memberships cm ON cm.id = i.created_by_membership_id
		  JOIN users u ON u.id = cm.user_id
		 WHERE i.token_hash = $1
		   AND p.status = 'active'
		   AND i.status = 'active'
		   AND i.expires_at > $2
		   AND i.used_count < i.max_uses`, hashToken(token), h.now()).Scan(&pv.PartyName, &pv.MemberCount, &pv.InviterDisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if err != nil {
		h.writeDomainError(w, r, "party.invite.preview", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, previewEnvelope{RequestID: httpapi.RequestID(r.Context()).String(), Preview: pv})
}

type acceptEnvelope struct {
	RequestID  string             `json:"request_id"`
	Party      partyResponse      `json:"party"`
	Membership membershipResponse `json:"membership"`
}

// acceptInvite는 T3다. 잠금 순서는 users -> parties -> party_invites다(§5.5).
// users를 먼저 잡는 이유는 같은 사용자의 서로 다른 Party 동시 수락이 참여 한도를
// 함께 넘지 못하게 직렬화하기 위해서다. T1(createParty)과 같은 순서라 교착이 없다.
func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// 요청 제한은 형식 검사·Prepare·mutation.Run(멱등성 replay 확인 포함)보다 먼저다.
	// 같은 Idempotency-Key의 재전송도 한도를 소비한다.
	if !h.enforceInviteLimit(w, r, ratelimit.ScopeInviteAccept, ratelimit.LimitAcceptUser, ratelimit.LimitAcceptIP) {
		return
	}
	token := r.PathValue("token")
	if token == "" || len(token) > maxTokenLength {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	prep, ok := mutation.Prepare(w, r, false)
	if !ok {
		return
	}
	now := h.now()
	tokenHash := hashToken(token)
	out, ok := h.run(w, r, p, prep, "party.invite.accept", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		if err := lockUserForPartyCreate(ctx, tx, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		// 1. token_hash로 party_id를 얻는다(잠금 없음).
		var inviteID, partyID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id, party_id FROM party_invites WHERE token_hash = $1`, tokenHash).Scan(&inviteID, &partyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return mutation.Result{}, errNotFound
		}
		if err != nil {
			return mutation.Result{}, err
		}
		// 2. Party를 잠그고 상태를 본다. 초대 상태 검사보다 반드시 앞선다.
		var partyStatus string
		var memberLimit int
		if err := tx.QueryRow(ctx, `SELECT status, member_limit FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&partyStatus, &memberLimit); err != nil {
			return mutation.Result{}, err
		}
		if partyStatus != "active" {
			return mutation.Result{}, errNotFound
		}
		// 3~4. 초대를 잠그고 저장된 status를 믿지 않고 만료를 다시 비교한다.
		var (
			status  string
			expires time.Time
			maxUses int16
			used    int16
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, expires_at, max_uses, used_count
			  FROM party_invites WHERE id = $1 FOR UPDATE`, inviteID).Scan(&status, &expires, &maxUses, &used); err != nil {
			return mutation.Result{}, err
		}
		if status != "active" || !expires.After(now) || used >= maxUses {
			return mutation.Result{}, errNotFound
		}
		// 5. 이미 활성 멤버.
		var already bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM party_memberships WHERE party_id = $1 AND user_id = $2 AND status = 'active')`,
			partyID, p.UserID).Scan(&already); err != nil {
			return mutation.Result{}, err
		}
		if already {
			return mutation.Result{}, errAlreadyMember
		}
		// 6. 정원. Party 잠금 아래에서 센다.
		var members int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM party_memberships WHERE party_id = $1 AND status = 'active'`, partyID).Scan(&members); err != nil {
			return mutation.Result{}, err
		}
		if members >= memberLimit {
			return mutation.Result{}, errPartyFull
		}
		// 7. 참여 Party 한도.
		var joined int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM party_memberships m JOIN parties pp ON pp.id = m.party_id
			 WHERE m.user_id = $1 AND m.status = 'active' AND pp.status = 'active'`, p.UserID).Scan(&joined); err != nil {
			return mutation.Result{}, err
		}
		if joined >= 20 {
			return mutation.Result{}, errJoinedLimitReached
		}
		// 8~10. 멤버십, 공개 수준, projection.
		membershipID, settingID := uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_memberships (id, party_id, user_id, role, invite_id, joined_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'member', $4, $5, $5, $5)`, membershipID, partyID, p.UserID, inviteID, now); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_visibility_settings (id, party_id, user_id, visibility_level)
			VALUES ($1, $2, $3, 'busyOnly')`, settingID, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if err := createBusyOnlyProjections(ctx, tx, partyID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		// 11. 사용 횟수. 소진되면 exhausted.
		if _, err := tx.Exec(ctx, `
			UPDATE party_invites
			   SET used_count = used_count + 1, last_used_at = $2, updated_at = $2,
			       status = CASE WHEN used_count + 1 >= max_uses THEN 'exhausted' ELSE status END
			 WHERE id = $1`, inviteID, now); err != nil {
			return mutation.Result{}, err
		}
		// 12. Party version.
		if _, err := tx.Exec(ctx, `UPDATE parties SET version = version + 1, updated_at = $2 WHERE id = $1`, partyID, now); err != nil {
			return mutation.Result{}, err
		}
		// 13. sync. 신규 멤버에게는 Party 단위 부트스트랩, 기존 멤버에게는 변경분, 방장에게는 초대 사용 횟수.
		if err := h.emitJoin(ctx, tx, partyID, inviteID, membershipID, settingID, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		// 14. 알림 job. payload에는 식별자만 담는다(§7.4).
		dedupe := membershipID.String()
		if err := tx.Enqueue(ctx, mutation.Job{
			Type:      jobMemberJoined,
			Payload:   map[string]string{"party_id": partyID.String(), "membership_id": membershipID.String()},
			DedupeKey: &dedupe,
		}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusCreated, ResourceType: entityPartyMembership, ResourceID: membershipID, ResourceVersion: 1}, nil
	})
	if !ok {
		return
	}

	// 재전송에서도 현재 상태를 먼저 평가한다(§9.4). 이미 떠났으면 과거 성공 payload를 주지 않는다.
	membership, err := h.loadMembershipForUser(r.Context(), out.ResourceID, p.UserID)
	if err != nil {
		h.writeDomainError(w, r, "party.invite.accept", err)
		return
	}
	party, err := loadPartyForMember(r.Context(), h.pool, membership.PartyID, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "대상을 찾을 수 없다")
		return
	}
	if err != nil {
		h.writeDomainError(w, r, "party.invite.accept", err)
		return
	}
	w.Header().Set("ETag", etag(party.Version))
	httpapi.WriteJSON(w, out.StatusCode, acceptEnvelope{
		RequestID: httpapi.RequestID(r.Context()).String(), Party: party, Membership: membership,
	})
}

func (h *Handler) emitJoin(ctx context.Context, tx *mutation.Tx, partyID, inviteID, membershipID, settingID, newUserID uuid.UUID) error {
	members, err := activeMemberships(ctx, tx, partyID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.userID == newUserID {
			continue
		}
		if err := emitProjection(ctx, tx, m.userID, partyID); err != nil {
			return err
		}
		if err := emitProjection(ctx, tx, m.userID, membershipID); err != nil {
			return err
		}
	}
	// 신규 멤버: party, 모든 party_membership, 본인 party_visibility_setting.
	if err := emitProjection(ctx, tx, newUserID, partyID); err != nil {
		return err
	}
	for _, m := range members {
		if err := emitProjection(ctx, tx, newUserID, m.id); err != nil {
			return err
		}
	}
	if err := emitProjection(ctx, tx, newUserID, settingID); err != nil {
		return err
	}
	// 방장에게만 초대 변경. token은 payload에 없다.
	for _, m := range members {
		if m.role == "owner" {
			return emitInviteUpsert(ctx, tx, m.userID, inviteID)
		}
	}
	return nil
}

type memberRef struct {
	id, userID uuid.UUID
	role       string
}

func activeMemberships(ctx context.Context, q Querier, partyID uuid.UUID) ([]memberRef, error) {
	rows, err := q.Query(ctx, `
		SELECT id, user_id, role FROM party_memberships
		 WHERE party_id = $1 AND status = 'active'
		 ORDER BY joined_at, id`, partyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memberRef
	for rows.Next() {
		var m memberRef
		if err := rows.Scan(&m.id, &m.userID, &m.role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func emitInviteUpsert(ctx context.Context, tx *mutation.Tx, recipient, inviteID uuid.UUID) error {
	p, err := InviteProjection(ctx, tx, inviteID)
	if err != nil {
		return err
	}
	return tx.EmitChange(ctx, mutation.Change{Recipient: recipient, EntityType: p.EntityType, EntityID: p.ID, Version: &p.Version, Payload: p.Payload})
}

// loadMembershipForUser는 수락 응답용 멤버십을 읽는다. 활성이 아니면 재전송이라도
// 성공 payload를 돌려주지 않는다. 해산된 Party면 410, 아니면 404다.
func (h *Handler) loadMembershipForUser(ctx context.Context, membershipID, userID uuid.UUID) (membershipResponse, error) {
	var m membershipResponse
	var joinedAt time.Time
	var partyStatus string
	err := h.pool.QueryRow(ctx, `
		SELECT m.id, m.party_id, m.user_id, u.display_name, m.role, m.status, m.version, m.joined_at, p.status
		  FROM party_memberships m
		  JOIN users u ON u.id = m.user_id
		  JOIN parties p ON p.id = m.party_id
		 WHERE m.id = $1 AND m.user_id = $2`, membershipID, userID).Scan(
		&m.ID, &m.PartyID, &m.UserID, &m.DisplayName, &m.Role, &m.Status, &m.Version, &joinedAt, &partyStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return membershipResponse{}, errNotFound
	}
	if err != nil {
		return membershipResponse{}, err
	}
	if partyStatus == "disbanded" {
		return membershipResponse{}, errPartyDisbanded
	}
	if m.Status != "active" {
		return membershipResponse{}, errNotFound
	}
	m.JoinedAt = joinedAt.UTC().Format(time.RFC3339)
	return m, nil
}

func loadInvite(ctx context.Context, q Querier, partyID, inviteID uuid.UUID, now time.Time) (inviteResponse, error) {
	row := q.QueryRow(ctx, `
		SELECT id, party_id, status, expires_at, max_uses, used_count, created_at
		  FROM party_invites WHERE id = $1 AND party_id = $2`, inviteID, partyID)
	return scanInvite(row, now)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanInvite(row rowScanner, now time.Time) (inviteResponse, error) {
	var inv inviteResponse
	var expires, created time.Time
	if err := row.Scan(&inv.ID, &inv.PartyID, &inv.Status, &expires, &inv.MaxUses, &inv.UsedCount, &created); err != nil {
		return inviteResponse{}, err
	}
	if inv.Status == "active" && !expires.After(now) {
		inv.Status = "expired"
	}
	inv.ExpiresAt = expires.UTC().Format(time.RFC3339)
	inv.CreatedAt = created.UTC().Format(time.RFC3339)
	return inv, nil
}

func (h *Handler) writeInviteLoadError(w http.ResponseWriter, r *http.Request, action string, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	h.writeDomainError(w, r, action, err)
}

// enforceInviteLimit는 사용자+IP 규칙을 한 번에 판정한다. 장치 오류 시 503(closed)이다.
// 사용자가 인증된 scope이므로 처음 한도를 넘긴 요청에는 audit 한 줄을 남긴다.
func (h *Handler) enforceInviteLimit(w http.ResponseWriter, r *http.Request, scope string, user, ip ratelimit.Limit) bool {
	p, _ := auth.PrincipalFrom(r.Context())
	d, ok := ratelimit.Enforce(w, r, h.limiter, ratelimit.ModeClosed,
		ratelimit.UserRule(scope, user, p.UserID),
		ratelimit.IPRule(r.Context(), scope, ip))
	if !ok {
		ratelimit.RecordExceeded(r.Context(), h.pool, p.UserID, d)
	}
	return ok
}
