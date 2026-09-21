package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/httpapi"
)

// Handler는 인증 endpoint와 인증 미들웨어를 제공한다.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler는 Handler를 만든다.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// Register는 인증 endpoint를 mux에 등록한다.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/apple", h.signInWithApple)
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	mux.Handle("POST /v1/auth/logout", h.Require(http.HandlerFunc(h.logout)))
}

type signInRequest struct {
	IdentityToken     string `json:"identity_token"`
	AuthorizationCode string `json:"authorization_code"`
	RawNonce          string `json:"raw_nonce"`
	DisplayName       string `json:"display_name"`
}

// sessionResponse는 로그인·refresh 응답이다. 시각은 §6에 따라 RFC 3339 UTC다.
type sessionResponse struct {
	RequestID             string `json:"request_id"`
	UserID                string `json:"user_id"`
	DeviceID              string `json:"device_id"`
	AccessToken           string `json:"access_token"`
	AccessTokenExpiresAt  string `json:"access_token_expires_at"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	NewUser               bool   `json:"new_user"`
}

func toSessionResponse(r *http.Request, s Session) sessionResponse {
	return sessionResponse{
		RequestID:             httpapi.RequestID(r.Context()).String(),
		UserID:                s.UserID.String(),
		DeviceID:              s.DeviceID.String(),
		AccessToken:           s.AccessToken,
		AccessTokenExpiresAt:  s.AccessTokenExpiresAt.UTC().Format(time.RFC3339),
		RefreshToken:          s.RefreshToken,
		RefreshTokenExpiresAt: s.RefreshTokenExpiresAt.UTC().Format(time.RFC3339),
		NewUser:               s.NewUser,
	}
}

func (h *Handler) signInWithApple(w http.ResponseWriter, r *http.Request) {
	var req signInRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "요청 본문이 올바르지 않다")
		return
	}
	if req.IdentityToken == "" || req.AuthorizationCode == "" || req.RawNonce == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"identity_token, authorization_code, raw_nonce가 필요하다")
		return
	}

	sess, err := h.svc.SignInWithApple(r.Context(), SignInInput{
		IdentityToken:     req.IdentityToken,
		AuthorizationCode: req.AuthorizationCode,
		RawNonce:          req.RawNonce,
		DisplayName:       req.DisplayName,
		RequestID:         httpapi.RequestID(r.Context()),
	})
	if err != nil {
		h.writeServiceError(w, r, "auth.apple_sign_in", err)
		return
	}
	h.logger.InfoContext(r.Context(), "로그인",
		slog.String("request_id", httpapi.RequestID(r.Context()).String()),
		slog.String("actor", sess.UserID.String()),
		slog.String("action", "auth.apple_sign_in"),
		slog.String("result", "success"),
	)
	httpapi.WriteJSON(w, http.StatusOK, toSessionResponse(r, sess))
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpapi.DecodeJSON(w, r, &req); err != nil || req.RefreshToken == "" {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "refresh_token이 필요하다")
		return
	}
	sess, err := h.svc.Refresh(r.Context(), req.RefreshToken, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeServiceError(w, r, "auth.refresh", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, toSessionResponse(r, sess))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	if err := h.svc.Logout(r.Context(), p, httpapi.RequestID(r.Context())); err != nil {
		h.writeServiceError(w, r, "auth.logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type principalKey struct{}

// PrincipalFrom은 Require를 통과한 요청의 principal을 돌려준다.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal은 principal을 context에 담는다. 테스트용이다.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Require는 인증이 필요한 handler를 감싼다. Authorization: Bearer <access token>.
func (h *Handler) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeInvalidSession, "인증이 필요하다")
			return
		}
		p, err := h.svc.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, ErrInvalidCredential) {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			}
			h.writeServiceError(w, r, "auth.authenticate", err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// writeServiceError는 서비스 오류를 §6 오류 응답으로 바꾼다. 예상하지 못한
// 오류의 원문은 로그에만 남기고 응답에는 넣지 않는다(§11).
func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, action string, err error) {
	var appleErr *appleid.APIError
	switch {
	case errors.Is(err, ErrInvalidCredential):
		httpapi.WriteError(w, r, http.StatusUnauthorized, httpapi.CodeInvalidSession, "세션이 유효하지 않다. 다시 로그인해야 한다")
	case errors.Is(err, ErrAccountLocked):
		httpapi.WriteError(w, r, http.StatusLocked, httpapi.CodeAccountLocked, "계정을 사용할 수 없는 상태다")
	case errors.Is(err, ErrUnavailable):
		h.logger.WarnContext(r.Context(), "외부 인증 서비스 장애",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", "failure"),
		)
		httpapi.WriteError(w, r, http.StatusServiceUnavailable, httpapi.CodeInternal, "인증 서비스를 일시적으로 사용할 수 없다")
	case errors.As(err, &appleErr):
		// Apple이 서버 자격을 거부했다. code는 Apple 문서의 고정 값이라 남겨도 된다.
		h.logger.ErrorContext(r.Context(), "Apple이 서버 자격을 거부했다. APPLE_TEAM_ID, APPLE_KEY_ID, APPLE_PRIVATE_KEY를 확인해야 한다",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", "failure"),
			slog.String("apple_error", appleErr.Code),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
	default:
		// pgx 오류 원문은 SQL과 값 일부를 담을 수 있다. 오류 "종류"만 남긴다.
		h.logger.ErrorContext(r.Context(), "인증 처리 실패",
			slog.String("request_id", httpapi.RequestID(r.Context()).String()),
			slog.String("action", action),
			slog.String("result", "failure"),
			slog.String("error_type", httpapi.ErrorType(err)),
		)
		httpapi.WriteError(w, r, http.StatusInternalServerError, httpapi.CodeInternal, "요청을 처리하지 못했다")
	}
}

// NormalizeDisplayName은 클라이언트가 보낸 표시 이름을 저장 가능한 형태로 만든다.
// 제어 문자와 서식 문자(Cf)를 지우고 앞뒤 공백을 자르며 길이를 제한한다. 남는
// 것이 없으면 ok가 false다. 가입은 기본 이름으로 대신하고, 이름 변경은 거부한다.
//
// Cf에는 U+202E 같은 bidi override가 있어, 남기면 다른 멤버 화면에서 이름 뒤의
// 텍스트 방향을 뒤집을 수 있다. 이모지 결합에 쓰는 ZWJ(U+200D)만 남긴다.
func NormalizeDisplayName(raw string) (name string, ok bool) {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (unicode.Is(unicode.Cf, r) && r != '\u200d') {
			return -1
		}
		return r
	}, strings.ToValidUTF8(raw, ""))
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "", false
	}
	if utf8.RuneCountInString(cleaned) > maxDisplayNameRunes {
		cleaned = strings.TrimSpace(string([]rune(cleaned)[:maxDisplayNameRunes]))
	}
	return cleaned, true
}
