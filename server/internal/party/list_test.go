package party

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func (s *stack) listParties(t *testing.T, token string) partyListEnvelope {
	t.Helper()
	rec := s.do(t, http.MethodGet, "/v1/parties", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/parties = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[partyListEnvelope](t, rec)
}

func (s *stack) createNamedParty(t *testing.T, token, key, name string) partyResponse {
	t.Helper()
	rec := s.mut(t, http.MethodPost, "/v1/parties", token, key, "", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	return decode[partyEnvelope](t, rec).Party
}

// 목록은 활성 멤버십이 있는 활성 Party만, 호출자 본인의 멤버십·활성 멤버 수와 함께 준다.
func TestListPartiesShowsOnlyActiveMemberships(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "list.owner."+uuid.NewString())
	member := s.signIn(t, "list.member."+uuid.NewString())
	stranger := s.signIn(t, "list.stranger."+uuid.NewString())

	kept := s.createNamedParty(t, owner.AccessToken, "list-kept", "Kept")
	left := s.createNamedParty(t, owner.AccessToken, "list-left", "Left")
	gone := s.createNamedParty(t, owner.AccessToken, "list-gone", "Gone")
	s.join(t, owner.AccessToken, kept.ID, member.AccessToken, "list-join-kept")
	s.join(t, owner.AccessToken, left.ID, member.AccessToken, "list-join-left")
	s.join(t, owner.AccessToken, gone.ID, member.AccessToken, "list-join-gone")
	if rec := s.leave(t, member.AccessToken, left.ID, "list-leave"); rec.Code != http.StatusOK {
		t.Fatalf("leave = %d: %s", rec.Code, rec.Body.String())
	}
	goneNow, err := loadPartyByID(t.Context(), s.pool, gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := s.disband(t, owner.AccessToken, gone.ID, "list-disband", goneNow.Version); rec.Code != http.StatusOK {
		t.Fatalf("disband = %d: %s", rec.Code, rec.Body.String())
	}

	got := s.listParties(t, member.AccessToken)
	if len(got.Parties) != 1 || got.Parties[0].Party.ID != kept.ID {
		t.Fatalf("member parties = %+v", got.Parties)
	}
	it := got.Parties[0]
	if it.Membership.UserID.String() != member.UserID || it.Membership.Role != "member" || it.Membership.Status != "active" ||
		it.Membership.PartyID != kept.ID || it.MemberCount != 2 || it.Party.Status != "active" || it.Party.Version < 2 {
		t.Fatalf("member item = %+v", it)
	}

	ownerList := s.listParties(t, owner.AccessToken)
	if len(ownerList.Parties) != 2 {
		t.Fatalf("owner parties = %d, want 2 (kept, left)", len(ownerList.Parties))
	}
	counts := map[uuid.UUID]int{}
	for _, it := range ownerList.Parties {
		if it.Membership.Role != "owner" || it.Membership.ID != it.Party.OwnerMembershipID {
			t.Fatalf("owner item = %+v", it)
		}
		counts[it.Party.ID] = it.MemberCount
	}
	if counts[kept.ID] != 2 || counts[left.ID] != 1 {
		t.Fatalf("member counts = %v", counts)
	}

	if got := s.listParties(t, stranger.AccessToken); got.Parties == nil || len(got.Parties) != 0 {
		t.Fatalf("stranger parties = %+v", got.Parties)
	}
	if rec := s.do(t, http.MethodGet, "/v1/parties", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("미인증 = %d", rec.Code)
	}
}
