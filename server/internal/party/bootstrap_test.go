package party

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestBootstrapProjectionsScopesPartyData(t *testing.T) {
	s := newStack(t)
	owner := s.signIn(t, "bootstrap.owner."+uuid.NewString())
	other := s.signIn(t, "bootstrap.other."+uuid.NewString())
	rec := s.mut(t, http.MethodPost, "/v1/parties", owner.AccessToken, "bootstrap-create", "", `{"name":"Bootstrap"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	partyID := decode[partyEnvelope](t, rec).Party.ID

	got, err := BootstrapProjections(context.Background(), s.pool, uuid.MustParse(owner.UserID))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, pr := range got {
		counts[pr.EntityType]++
	}
	if counts[entityParty] != 1 || counts[entityPartyMembership] != 1 || counts[entityVisibilitySetting] != 1 {
		t.Fatalf("owner bootstrap entities = %v", counts)
	}
	if got[0].ID != partyID {
		t.Fatalf("first party = %s, want %s", got[0].ID, partyID)
	}

	got, err = BootstrapProjections(context.Background(), s.pool, uuid.MustParse(other.UserID))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("nonmember received %d Party entities", len(got))
	}
}
