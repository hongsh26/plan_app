package party

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"plantogether/server/internal/platform/httpapi"
)

func TestVisibilitySelfAPIAndImmediateHiddenProjectionRemoval(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "visibility.owner."+uuid.NewString())
	member := s.signIn(t, "visibility.member."+uuid.NewString())
	outsider := s.signIn(t, "visibility.outsider."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "visibility-party")
	invite := s.mustInvite(t, owner.AccessToken, partyID, "visibility-invite", "")
	if rec := s.accept(t, member.AccessToken, invite.Token, "visibility-accept"); rec.Code != http.StatusCreated {
		t.Fatalf("가입 = %d: %s", rec.Code, rec.Body.String())
	}
	path := "/v1/parties/" + partyID.String() + "/visibility/me"
	if got := s.do(t, http.MethodGet, path, outsider.AccessToken, nil); got.Code != http.StatusNotFound {
		t.Fatalf("비멤버 조회 = %d: %s", got.Code, got.Body.String())
	}
	initial := s.do(t, http.MethodGet, path, owner.AccessToken, nil)
	if initial.Code != http.StatusOK {
		t.Fatalf("본인 조회 = %d: %s", initial.Code, initial.Body.String())
	}
	setting := decode[visibilityEnvelope](t, initial).Visibility
	if setting.VisibilityLevel != visibilityBusyOnly || setting.Version != 1 || setting.ShareLocation {
		t.Fatalf("기본 설정 = %+v", setting)
	}

	projectionID := uuid.New()
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO party_schedule_projections
		(id, party_id, owner_user_id, source_event_key, source_generation, setting_version,
		 visibility_level, start_at, end_at, time_zone)
		VALUES ($1, $2, $3, decode(repeat('aa', 16), 'hex'), 1, 1,
		        'busyOnly', $4, $5, 'Asia/Seoul')`,
		projectionID, partyID, uuid.MustParse(owner.UserID), time.Now().UTC(), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(key, version, body string) int {
		t.Helper()
		return s.mut(t, http.MethodPatch, path, owner.AccessToken, key, version, body).Code
	}
	if code := mutate("visibility-invalid", `"1"`, `{"visibility_level":"hidden","share_location":true}`); code != http.StatusBadRequest {
		t.Fatalf("잘못된 hidden 설정 = %d", code)
	}
	if code := mutate("visibility-missing", `"1"`, `{"visibility_level":"hidden"}`); code != http.StatusBadRequest {
		t.Fatalf("share_location 누락 = %d", code)
	}
	if code := mutate("visibility-hidden", `"1"`, `{"visibility_level":"hidden","share_location":false}`); code != http.StatusOK {
		t.Fatalf("hidden 전환 = %d", code)
	}
	assertCount(t, s.pool, `SELECT count(*) FROM party_schedule_projections WHERE id = $1`, projectionID, 0)
	for _, userID := range []string{owner.UserID, member.UserID} {
		if n := changeCount(t, s, userID, entityScheduleProjection, "tombstone", projectionID); n != 1 {
			t.Fatalf("%s에게 projection 삭제 변경 %d건", userID, n)
		}
		var tombstoneVersion int64
		if err := s.pool.QueryRow(context.Background(), `
			SELECT entity_version FROM sync_changes
			 WHERE recipient_user_id = $1 AND entity_type = 'party_schedule_projection'
			   AND entity_id = $2 AND operation = 'tombstone'`, userID, projectionID).Scan(&tombstoneVersion); err != nil {
			t.Fatal(err)
		}
		if tombstoneVersion != 2 {
			t.Fatalf("삭제 version = %d, want 2", tombstoneVersion)
		}
	}
	if code := mutate("visibility-hidden", `"1"`, `{"visibility_level":"hidden","share_location":false}`); code != http.StatusOK {
		t.Fatalf("멱등 재전송 = %d", code)
	}
	if code := mutate("visibility-stale", `"1"`, `{"visibility_level":"busyOnly","share_location":false}`); code != http.StatusConflict {
		t.Fatalf("낡은 version = %d", code)
	}
	if code := mutate("visibility-details", `"2"`, `{"visibility_level":"details","share_location":false}`); code != http.StatusAccepted {
		t.Fatalf("details 요청 = %d", code)
	}
	latest := decode[visibilityEnvelope](t, s.do(t, http.MethodGet, path, owner.AccessToken, nil)).Visibility
	if latest.Version != 3 || latest.DetailsRefreshState != detailsRefreshPending {
		t.Fatalf("details 대기 상태 = %+v", latest)
	}
	if got := s.do(t, http.MethodGet, path, member.AccessToken, nil); got.Code != http.StatusOK || decode[visibilityEnvelope](t, got).Visibility.UserID != uuid.MustParse(member.UserID) {
		t.Fatalf("다른 멤버 본인 조회 = %d: %s", got.Code, got.Body.String())
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE party_memberships SET status = 'left', ended_at = now(), end_reason = 'left_voluntarily' WHERE party_id = $1 AND user_id = $2`, partyID, uuid.MustParse(member.UserID)); err != nil {
		t.Fatal(err)
	}
	if got := s.do(t, http.MethodGet, path, member.AccessToken, nil); got.Code != http.StatusNotFound {
		t.Fatalf("과거 멤버 조회 = %d: %s", got.Code, got.Body.String())
	}
	assertError(t, s.mut(t, http.MethodPatch, path, outsider.AccessToken, "visibility-outside", `"1"`, `{"visibility_level":"hidden","share_location":false}`), http.StatusNotFound, httpapi.CodeNotFound)
}

func TestScheduleScopeAndSafeDetailFallback(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "schedule.owner."+uuid.NewString())
	member := s.signIn(t, "schedule.member."+uuid.NewString())
	outsider := s.signIn(t, "schedule.outsider."+uuid.NewString())
	partyID := s.createParty(t, owner.AccessToken, "schedule-party")
	invite := s.mustInvite(t, owner.AccessToken, partyID, "schedule-invite", "")
	if rec := s.accept(t, member.AccessToken, invite.Token, "schedule-accept"); rec.Code != http.StatusCreated {
		t.Fatalf("가입 = %d: %s", rec.Code, rec.Body.String())
	}
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(time.Hour)
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO party_schedule_projections
		(id, party_id, owner_user_id, source_event_key, source_generation, setting_version,
		 visibility_level, start_at, end_at, time_zone, title_ciphertext)
		VALUES ($1, $2, $3, decode(repeat('bb', 16), 'hex'), 1, 1,
		        'details', $4, $5, 'Asia/Seoul', decode('01', 'hex'))`,
		uuid.New(), partyID, uuid.MustParse(owner.UserID), start, end)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/v1/parties/%s/schedule?start_at=%s&end_at=%s", partyID,
		start.Add(-time.Hour).Format(time.RFC3339), end.Add(time.Hour).Format(time.RFC3339))
	if got := s.do(t, http.MethodGet, path, outsider.AccessToken, nil); got.Code != http.StatusNotFound {
		t.Fatalf("비멤버 일정 조회 = %d: %s", got.Code, got.Body.String())
	}
	got := s.do(t, http.MethodGet, path, member.AccessToken, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("멤버 일정 조회 = %d: %s", got.Code, got.Body.String())
	}
	items := decode[scheduleEnvelope](t, got).Projections
	if len(items) != 1 || items[0].DisplayKind != visibilityBusyOnly || items[0].Title != nil || items[0].Location != nil {
		t.Fatalf("상세 복호화가 없는 조회가 안전하게 강등되지 않았다: %+v", items)
	}
	if got := s.do(t, http.MethodGet, "/v1/parties/"+partyID.String()+"/schedule?start_at=bad", member.AccessToken, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("잘못된 범위 = %d", got.Code)
	}
}
