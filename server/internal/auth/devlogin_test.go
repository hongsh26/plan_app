package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"plantogether/server/internal/platform/appleid"
)

type recordingVerifier struct{ called bool }

func (r *recordingVerifier) Verify(_ context.Context, idToken, _ string) (appleid.Identity, error) {
	r.called = true
	return appleid.Identity{Subject: "real:" + idToken}, nil
}

func TestDevVerifierAcceptsOnlyDevTokensAndDelegatesTheRest(t *testing.T) {
	next := &recordingVerifier{}
	v := DevVerifier{Next: next}
	ctx := context.Background()

	id, err := v.Verify(ctx, "dev:alice.kim", "n")
	if err != nil || id.Subject != "dev:alice.kim" || next.called {
		t.Fatalf("id=%+v err=%v called=%v", id, err, next.called)
	}
	max := "dev:" + strings.Repeat("a", 32)
	if _, err := v.Verify(ctx, max, "n"); err != nil {
		t.Errorf("32자 이름을 거부했다: %v", err)
	}
	for _, bad := range []string{
		"dev:", "dev:Alice", "dev:a b", "dev:../x",
		"dev:" + strings.Repeat("a", 33), // 길이 경계
		"dev:alice\n", "dev:ａlice", "dev:alice\x00",
	} {
		if _, err := v.Verify(ctx, bad, "n"); err == nil {
			t.Errorf("%q를 받았다", bad)
		}
	}
	if next.called {
		t.Fatal("dev: 접두사 token이 실제 검증기로 넘어갔다")
	}
	id, err = v.Verify(ctx, "eyJ.real.token", "n")
	if err != nil || !next.called || id.Subject != "real:eyJ.real.token" {
		t.Fatalf("위임 실패: %+v %v", id, err)
	}
}

func TestDevVerifierWithoutNextRejectsRealTokens(t *testing.T) {
	// 개발 모드(Next 없음)에서는 실제 token은 물론 대문자 접두사 같은 변형도 받지 않는다.
	for _, tok := range []string{"eyJ.real.token", "DEV:alice", "Dev:alice", ""} {
		if _, err := (DevVerifier{}).Verify(context.Background(), tok, "n"); err == nil {
			t.Fatalf("%q를 받았다", tok)
		}
	}
	if errors.Is(errors.New("x"), appleid.ErrKeysUnavailable) {
		t.Fatal("unreachable")
	}
}
