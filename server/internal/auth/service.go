// Package auth는 account_backend_design.md §5의 로그인과 세션을 구현한다.
//
//   - POST /v1/auth/apple   : Apple credential 검증, 가입/로그인 (§5.1)
//   - POST /v1/auth/refresh : refresh token 회전과 재사용 탐지 (§5.2)
//   - POST /v1/auth/logout  : 현재 기기 세션 폐기 (§5.2)
//   - 인증 미들웨어         : access token + 기기·계정 상태 확인 (§5.3, §14)
//
// 인증 endpoint는 §6의 Idempotency-Key 규칙 밖이다. idempotency_keys는 user_id와
// device_id를 NOT NULL FK로 요구하는데, 로그인 요청 시점에는 둘 다 없거나(가입)
// access token이 없어 신뢰할 수 없다. 로그인의 중복 방지는
// auth_identities_provider_subject_key와 Apple code의 1회성이, refresh의 중복
// 방지는 회전 자체가 맡는다.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/platform/appleid"
)

var (
	// ErrInvalidCredential은 Apple credential, access token, refresh token이
	// 무효일 때다. HTTP 401 invalid_session. 원인(만료, 폐기, 위조, 재사용)을
	// 클라이언트에 구분해 알리지 않는다.
	ErrInvalidCredential = errors.New("auth: 자격이 유효하지 않다")

	// ErrAccountLocked는 계정이 active가 아닐 때다. HTTP 423 account_locked.
	ErrAccountLocked = errors.New("auth: 계정이 잠겨 있다")

	// ErrUnavailable은 Apple 같은 외부 의존성 장애로 판단할 수 없을 때다.
	// HTTP 503. 사용자의 자격이 틀렸다고 말하지 않는다.
	ErrUnavailable = errors.New("auth: 외부 인증 서비스를 사용할 수 없다")
)

// AppleVerifier는 identity token 검증이다. appleid.Verifier가 구현한다.
type AppleVerifier interface {
	Verify(ctx context.Context, idToken, rawNonce string) (appleid.Identity, error)
}

// AppleCodeExchanger는 authorization code 교환이다. appleid.Client가 구현한다.
type AppleCodeExchanger interface {
	ExchangeCode(ctx context.Context, code string) (appleid.Exchange, error)
}

// Sealer는 DB에 저장할 비밀 값의 암호화다. secretbox가 구현한다.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
}

// SealPurposeAppleRefreshToken은 Apple refresh token 암호문의 associated data다.
// 복호화하는 삭제 worker가 같은 값을 써야 한다.
const SealPurposeAppleRefreshToken = "auth_identities.provider_refresh_token"

// defaultDisplayName은 Apple이 이름을 주지 않았을 때의 초기 표시 이름이다.
// Apple은 이름을 최초 동의 때 한 번만, 그것도 클라이언트에만 준다.
const defaultDisplayName = "새 사용자"

// maxDisplayNameRunes는 표시 이름 길이 상한이다.
const maxDisplayNameRunes = 50

// Service는 로그인과 세션 상태 전이를 담당한다.
type Service struct {
	pool   *pgxpool.Pool
	apple  AppleVerifier
	code   AppleCodeExchanger
	sealer Sealer
	tokens *AccessTokens
	now    func() time.Time
}

// Deps는 Service의 의존성이다.
//
// CodeExchanger와 Sealer는 함께 있거나 함께 없어야 한다. 둘 다 없으면 Apple
// code 교환을 건너뛴다. 이는 Apple 자격이 없는 로컬 개발에서만 허용되며 그 판단은
// config가 한다. 교환한 token을 암호화 없이 저장하는 조합은 만들 수 없다.
type Deps struct {
	Pool          *pgxpool.Pool
	Apple         AppleVerifier
	CodeExchanger AppleCodeExchanger
	Sealer        Sealer
	Tokens        *AccessTokens
	Now           func() time.Time
}

// NewService는 Service를 만든다.
func NewService(d Deps) (*Service, error) {
	if d.Pool == nil || d.Apple == nil || d.Tokens == nil {
		return nil, errors.New("auth: Pool, Apple, Tokens는 필수다")
	}
	if (d.CodeExchanger == nil) != (d.Sealer == nil) {
		return nil, errors.New("auth: CodeExchanger와 Sealer는 함께 설정해야 한다")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{pool: d.Pool, apple: d.Apple, code: d.CodeExchanger, sealer: d.Sealer, tokens: d.Tokens, now: d.Now}, nil
}

// SignInInput은 POST /v1/auth/apple의 입력이다.
type SignInInput struct {
	IdentityToken     string
	AuthorizationCode string
	RawNonce          string
	// DisplayName은 Apple이 최초 동의 때 클라이언트에 준 이름이다. 신규 가입에만
	// 쓰고 기존 사용자의 이름을 덮어쓰지 않는다(§5.1 "프로필 초기값으로만").
	DisplayName string
	RequestID   uuid.UUID
}

// Session은 로그인·refresh 결과로 클라이언트에 주는 값이다.
type Session struct {
	UserID                uuid.UUID
	DeviceID              uuid.UUID
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
	// NewUser는 이번 로그인으로 계정이 만들어졌는지다.
	NewUser bool
}

// SignInWithApple은 §5.1 흐름을 수행한다. 새 기기 ID를 발급하고 세션을 연다.
func (s *Service) SignInWithApple(ctx context.Context, in SignInInput) (Session, error) {
	identity, err := s.apple.Verify(ctx, in.IdentityToken, in.RawNonce)
	if err != nil {
		if errors.Is(err, appleid.ErrKeysUnavailable) {
			return Session{}, ErrUnavailable
		}
		s.auditFailure(ctx, "auth.apple_sign_in", in.RequestID)
		return Session{}, ErrInvalidCredential
	}

	// code는 1회용이고 5분 유효이므로 DB 트랜잭션 전에 즉시 교환한다. 교환이
	// 실패하면 로그인도 실패시킨다. 교환 없이 가입시키면 §10 3단계의 revoke에
	// 쓸 token이 없는 계정이 생긴다.
	//
	// 계정이 잠겨 있어도 교환은 먼저 일어난다. 상태는 DB 트랜잭션 안에서만 확정할
	// 수 있고, 교환은 트랜잭션 밖에서 해야 하기 때문이다(외부 호출 동안 행 잠금을
	// 쥐지 않는다). 그 경우 받은 Apple token은 저장하지 않고 버린다.
	var sealedAppleToken []byte
	if s.code != nil {
		ex, err := s.code.ExchangeCode(ctx, in.AuthorizationCode)
		if err != nil {
			var apiErr *appleid.APIError
			if errors.As(err, &apiErr) {
				if apiErr.IsUserError() {
					s.auditFailure(ctx, "auth.apple_code_exchange", in.RequestID)
					return Session{}, ErrInvalidCredential
				}
				// invalid_client 등은 서버 자격 문제다. 사용자에게 다시 로그인하라고
				// 답하면 모든 로그인이 401로 보이고 운영자는 원인을 모른다.
				return Session{}, fmt.Errorf("auth: Apple이 서버 자격을 거부했다: %w", apiErr)
			}
			return Session{}, ErrUnavailable
		}
		// code는 identity token과 따로 제출된다. 남의 code를 자기 identity token과
		// 함께 내면 남의 Apple grant가 내 계정에 저장되고, §10 삭제 때 엉뚱한
		// grant를 revoke하게 된다.
		if ex.Subject != identity.Subject {
			s.auditFailure(ctx, "auth.apple_code_subject_mismatch", in.RequestID)
			return Session{}, ErrInvalidCredential
		}
		sealedAppleToken, err = s.sealer.Seal([]byte(ex.RefreshToken), SealPurposeAppleRefreshToken)
		if err != nil {
			return Session{}, fmt.Errorf("auth: Apple refresh token을 암호화할 수 없다: %w", err)
		}
	}

	displayName := normalizeDisplayName(in.DisplayName)

	// 같은 subject의 최초 로그인이 동시에 두 번 오면 한쪽의 identity INSERT가
	// unique index에서 기다렸다가 DO NOTHING이 된다. 그 트랜잭션을 버리고 처음부터
	// 다시 하면 상대가 커밋한 계정을 찾는다. 두 번이면 충분하다. 세 번째에도
	// 못 찾으면 상대가 계속 롤백하는 이상 상황이다.
	for attempt := 0; attempt < 3; attempt++ {
		sess, retry, err := s.signInTx(ctx, identity.Subject, displayName, sealedAppleToken, in.RequestID)
		if retry {
			continue
		}
		return sess, err
	}
	return Session{}, errors.New("auth: 동시 가입 경쟁이 해소되지 않았다")
}

func (s *Service) signInTx(ctx context.Context, subject, displayName string, sealedAppleToken []byte, requestID uuid.UUID) (sess Session, retry bool, err error) {
	now := s.now()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var userID uuid.UUID
		var status string
		newUser := false
		// FOR SHARE OF u: 로그인과 계정 삭제 요청이 겹칠 때 삭제의 상태 UPDATE가
		// 이 트랜잭션을 기다리게 한다. 삭제가 먼저 커밋됐다면 여기서
		// deletion_requested를 보고 거부한다. 로그인이 먼저 커밋됐다면 삭제가 이
		// 세션까지 폐기한다. 어느 순서든 삭제된 계정에 살아 있는 세션이 남지 않는다.
		err := tx.QueryRow(ctx, `
			SELECT u.id, u.status
			  FROM auth_identities i
			  JOIN users u ON u.id = i.user_id
			 WHERE i.provider = 'apple' AND i.provider_subject = $1
			   FOR SHARE OF u`, subject).Scan(&userID, &status)

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			userID = uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, displayName); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO auth_identities (id, user_id, provider, provider_subject, provider_refresh_token_ciphertext)
				VALUES ($1, $2, 'apple', $3, $4)
				ON CONFLICT (provider, provider_subject) DO NOTHING`,
				uuid.New(), userID, subject, sealedAppleToken)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				retry = true
				return errRollback
			}
			newUser = true

		case err != nil:
			return err

		default:
			// §5.3: active가 아닌 계정은 로그인할 수 없다. deleted는 §10 7단계에서
			// identity row가 함께 지워지므로 여기에 오지 않고 새 가입이 된다.
			if status != "active" {
				return ErrAccountLocked
			}
			if sealedAppleToken != nil {
				if _, err := tx.Exec(ctx, `
					UPDATE auth_identities
					   SET provider_refresh_token_ciphertext = $2, updated_at = now()
					 WHERE provider = 'apple' AND provider_subject = $1`,
					subject, sealedAppleToken); err != nil {
					return err
				}
			}
		}

		deviceID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO devices (id, user_id, platform, last_seen_at)
			VALUES ($1, $2, 'ios', $3)`, deviceID, userID, now); err != nil {
			return err
		}

		sess, err = s.openSession(ctx, tx, userID, deviceID, uuid.New(), now)
		if err != nil {
			return err
		}
		sess.NewUser = newUser
		return audit(ctx, tx, &userID, "auth.apple_sign_in", "user", &userID, "success", requestID)
	})
	if errors.Is(err, errRollback) {
		return Session{}, retry, nil
	}
	if err != nil {
		if errors.Is(err, ErrAccountLocked) {
			s.auditFailure(ctx, "auth.apple_sign_in", requestID)
		}
		return Session{}, false, err
	}
	return sess, false, nil
}

// errRollback은 BeginFunc 안에서 오류 없이 롤백하려고 쓰는 표시다.
var errRollback = errors.New("auth: rollback")

// openSession은 sessions row를 만들고 token 쌍을 발급한다. 호출자의 트랜잭션
// 안에서 실행된다.
func (s *Service) openSession(ctx context.Context, tx pgx.Tx, userID, deviceID, familyID uuid.UUID, now time.Time) (Session, error) {
	refresh, hash, err := newRefreshToken()
	if err != nil {
		return Session{}, err
	}
	refreshExp := now.Add(RefreshTokenTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (id, user_id, device_id, token_family_id, refresh_token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), userID, deviceID, familyID, hash, refreshExp); err != nil {
		return Session{}, err
	}
	access, accessExp, err := s.tokens.Issue(Principal{UserID: userID, DeviceID: deviceID})
	if err != nil {
		return Session{}, err
	}
	return Session{
		UserID:                userID,
		DeviceID:              deviceID,
		AccessToken:           access,
		AccessTokenExpiresAt:  accessExp,
		RefreshToken:          refresh,
		RefreshTokenExpiresAt: refreshExp,
	}, nil
}

// Refresh는 §5.2의 refresh token 회전이다.
//
// 이미 사용된 token이 다시 오면 탈취로 간주한다. 같은 token family의 세션을 모두
// 폐기하고 기기도 폐기한다. 기기를 폐기하지 않으면 탈취한 쪽이 가진 access
// token이 만료될 때까지 최대 15분 더 쓰인다. 폐기는 반드시 커밋해야 하므로
// 오류를 돌려주더라도 트랜잭션은 커밋한다.
//
// 정상 클라이언트가 응답을 잃고 같은 token으로 재시도해도 재사용으로 판정된다.
// §5.2가 요구하는 엄격한 동작이며, 그 경우 사용자는 다시 로그인한다.
func (s *Service) Refresh(ctx context.Context, refreshToken string, requestID uuid.UUID) (Session, error) {
	if !wellFormedRefreshToken(refreshToken) {
		return Session{}, ErrInvalidCredential
	}
	now := s.now()
	hash := hashRefreshToken(refreshToken)

	var result Session
	var outcome error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// 잠금 순서는 모든 인증 경로에서 users → devices → sessions다(lockOrder 참고).
		// 세션 row를 먼저 잠그면 같은 기기의 로그아웃이나 계정 삭제와 교착한다.
		// 그래서 잠그지 않고 소유자를 먼저 읽은 뒤, 순서대로 잠그고 다시 읽는다.
		var ownerUser, ownerDevice uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT user_id, device_id FROM sessions WHERE refresh_token_hash = $1`, hash).Scan(&ownerUser, &ownerDevice)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = ErrInvalidCredential
			return nil
		}
		if err != nil {
			return err
		}
		if err := lockUserAndDevice(ctx, tx, ownerUser, ownerDevice); err != nil {
			return err
		}

		var (
			sessionID, userID, deviceID, familyID uuid.UUID
			expiresAt                             time.Time
			usedAt, revokedAt, deviceRevokedAt    *time.Time
			status                                string
		)
		// FOR UPDATE OF s: 같은 token으로 동시에 두 요청이 오면 뒤의 요청은 앞의
		// 커밋을 기다렸다가 used_at을 보고 재사용으로 판정된다. 둘 다 새 token을
		// 받는 경로가 없다. 기기 잠금이 이미 직렬화하지만, 세션 row 자체의 잠금을
		// 기기 잠금의 부수 효과에 맡기지 않는다.
		err = tx.QueryRow(ctx, `
			SELECT s.id, s.user_id, s.device_id, s.token_family_id, s.expires_at,
			       s.used_at, s.revoked_at, d.revoked_at, u.status
			  FROM sessions s
			  JOIN devices d ON d.id = s.device_id
			  JOIN users u ON u.id = s.user_id
			 WHERE s.refresh_token_hash = $1
			   FOR UPDATE OF s`, hash).Scan(
			&sessionID, &userID, &deviceID, &familyID, &expiresAt,
			&usedAt, &revokedAt, &deviceRevokedAt, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			// 먼저 읽은 뒤 잠그기 전에 세션이 사라졌다(기기·사용자 삭제의 cascade나
			// 세션 정리). 없는 세션이므로 무효 자격이다.
			outcome = ErrInvalidCredential
			return nil
		}
		if err != nil {
			return err
		}

		if usedAt != nil {
			// token family는 기기 하나 안에서만 생긴다(로그인이 새 기기와 새 family를
			// 함께 만들고 refresh는 기기를 바꾸지 않는다). 그래서 기기 폐기가 family
			// 전체 폐기를 포함한다.
			if err := revokeDevice(ctx, tx, deviceID); err != nil {
				return err
			}
			outcome = ErrInvalidCredential
			return audit(ctx, tx, &userID, "auth.refresh_reuse_detected", "device", &deviceID, "failure", requestID)
		}
		if revokedAt != nil || deviceRevokedAt != nil || !now.Before(expiresAt) {
			outcome = ErrInvalidCredential
			return nil
		}
		if status != "active" {
			outcome = ErrAccountLocked
			return nil
		}

		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET used_at = $2, updated_at = now() WHERE id = $1`, sessionID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE devices SET last_seen_at = $2, updated_at = now() WHERE id = $1`, deviceID, now); err != nil {
			return err
		}
		result, err = s.openSession(ctx, tx, userID, deviceID, familyID, now)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if outcome != nil {
		return Session{}, outcome
	}
	return result, nil
}

// lockOrder: 인증 경로가 여러 테이블의 row를 잠글 때의 순서는
//
//	users(FOR SHARE, 삭제·상태 변경은 FOR UPDATE) → devices(FOR UPDATE) → sessions
//
// 이다. 계정 삭제(§10)를 구현할 때도 이 순서를 따라야 로그인·refresh·로그아웃과
// 교착하지 않는다.
//
// lockUserAndDevice는 앞의 두 단계를 수행한다. users를 FOR SHARE로 잡아 계정 삭제·
// 잠금과 겹칠 때 어느 쪽이 먼저 커밋하든 상태를 일관되게 보게 한다.
func lockUserAndDevice(ctx context.Context, tx pgx.Tx, userID, deviceID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR SHARE`, userID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT 1 FROM devices WHERE id = $1 FOR UPDATE`, deviceID)
	return err
}

// revokeDevice는 기기와 그 기기의 모든 세션을 폐기한다.
//
// 기기 row의 version을 올린다. devices는 클라이언트가 expected_version으로
// 수정하는 대상이므로(§6), 서버 쪽 상태 변경도 version을 올려야 클라이언트의
// 낡은 수정이 version_conflict로 걸린다.
func revokeDevice(ctx context.Context, tx pgx.Tx, deviceID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET revoked_at = now(), updated_at = now()
		 WHERE device_id = $1 AND revoked_at IS NULL`, deviceID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE devices SET revoked_at = now(), version = version + 1, updated_at = now()
		 WHERE id = $1 AND revoked_at IS NULL`, deviceID)
	return err
}

// Logout은 현재 기기의 세션을 폐기한다(§5.2). 다른 기기의 세션은 그대로다.
//
// 기기 자체도 폐기한다. device ID는 로그인할 때마다 서버가 새로 발급하므로
// 로그아웃한 기기 ID는 다시 쓰이지 않는다. 폐기하지 않으면 그 기기로 보낸
// access token이 만료까지 남고, 이후 푸시 대상에도 남는다.
//
// 이미 폐기된 기기에 다시 호출해도 성공한다.
func (s *Service) Logout(ctx context.Context, p Principal, requestID uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockUserAndDevice(ctx, tx, p.UserID, p.DeviceID); err != nil {
			return err
		}
		if err := revokeDevice(ctx, tx, p.DeviceID); err != nil {
			return err
		}
		return audit(ctx, tx, &p.UserID, "auth.logout", "device", &p.DeviceID, "success", requestID)
	})
}

// Authenticate는 access token을 검증하고 기기·계정 상태를 DB에서 확인한다.
//
// access token만 믿지 않는다. 로그아웃·재사용 탐지·계정 잠금은 즉시 효력이
// 있어야 하는데(§5.2 "모든 세션을 즉시 폐기", §14 "보호 API를 사용할 수 없다"),
// 서명된 token은 발급 후 15분 동안 스스로 무효가 되지 않는다.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (Principal, error) {
	p, err := s.tokens.Verify(accessToken)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	var status string
	var deviceRevoked bool
	err = s.pool.QueryRow(ctx, `
		SELECT u.status, d.revoked_at IS NOT NULL
		  FROM devices d
		  JOIN users u ON u.id = d.user_id
		 WHERE d.id = $1 AND d.user_id = $2`, p.DeviceID, p.UserID).Scan(&status, &deviceRevoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidCredential
	}
	if err != nil {
		return Principal{}, err
	}
	if deviceRevoked {
		return Principal{}, ErrInvalidCredential
	}
	if status != "active" {
		return Principal{}, ErrAccountLocked
	}
	return p, nil
}

// audit는 보안 audit 이벤트를 같은 트랜잭션에 남긴다. §11에 따라 token,
// Apple credential을 넣지 않는다.
func audit(ctx context.Context, tx pgx.Tx, actor *uuid.UUID, action, targetType string, targetID *uuid.UUID, result string, requestID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (id, actor_user_id, action, target_type, target_id, result, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), actor, action, targetType, targetID, result, nullableUUID(requestID))
	return err
}

// auditFailure는 행위자를 모르는 실패를 남긴다(audit_events 스키마 주석 참고).
// 로그인 실패가 audit 기록 실패 때문에 다른 오류로 바뀌면 안 되므로 결과를
// 무시한다.
func (s *Service) auditFailure(ctx context.Context, action string, requestID uuid.UUID) {
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO audit_events (id, action, result, request_id)
		VALUES ($1, $2, 'failure', $3)`, uuid.New(), action, nullableUUID(requestID))
}

func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
