package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"plantogether/server/internal/platform/appleid"
)

// devPrefix는 개발용 identity token의 접두사다. 실제 Apple subject는 이 접두사로 시작하지
// 않으므로 개발 계정과 실제 계정이 충돌하지 않는다.
const devPrefix = "dev:"

var devNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)

// DevVerifier는 시연과 로컬 개발용 검증기다. identity token이 "dev:<이름>"이면 Apple 서버
// 없이 그 이름을 subject로 로그인시킨다. 그 밖의 token은 Next(실제 검증기)에 넘긴다.
//
// APP_ENV=local에서만 만들어져야 한다. config.LoadAuth가 local 밖에서는 인증 설정 자체를
// 거부하므로 배포 환경에서는 이 검증기에 도달할 수 없다.
type DevVerifier struct {
	Next AppleVerifier
}

// Verify는 AppleVerifier를 구현한다.
func (d DevVerifier) Verify(ctx context.Context, idToken, rawNonce string) (appleid.Identity, error) {
	if !strings.HasPrefix(idToken, devPrefix) {
		if d.Next == nil {
			return appleid.Identity{}, errors.New("auth: 개발용 로그인만 켜져 있다")
		}
		return d.Next.Verify(ctx, idToken, rawNonce)
	}
	name := strings.TrimPrefix(idToken, devPrefix)
	if !devNameRe.MatchString(name) {
		return appleid.Identity{}, errors.New("auth: 개발용 이름은 [a-z0-9._-] 1~32자여야 한다")
	}
	return appleid.Identity{Subject: idToken}, nil
}
