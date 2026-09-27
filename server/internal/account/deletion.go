package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plantogether/server/internal/auth"
	"plantogether/server/internal/platform/appleid"
	"plantogether/server/internal/platform/audit"
	"plantogether/server/internal/platform/httpapi"
	"plantogether/server/internal/platform/jobs"
	"plantogether/server/internal/platform/mutation"
)

// AccountDeletionJobType은 §10 계정 삭제 pipeline의 outbox job 종류다.
const AccountDeletionJobType = "account_deletion"

type accountDeletionPayload struct {
	UserID string `json:"user_id"`
}

// deleteMe는 §10 계정 삭제 요청이다. 요청 즉시 보호 API 접근을 막고 첫 삭제 job을
// enqueue한다. 같은 요청을 성공 뒤 재전송하면 인증 계층에서 이미 차단된다.
func (h *Handler) deleteMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	prep, ok := mutation.Prepare(w, r, true)
	if !ok {
		return
	}
	if len(prep.Body) != 0 {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeInvalidRequest, "본문을 보내지 않는다")
		return
	}

	out, ok := h.run(w, r, p, prep, "account.delete_request", func(ctx context.Context, tx *mutation.Tx) (mutation.Result, error) {
		var current int64
		var status string
		if err := tx.QueryRow(ctx, `
			SELECT version, status FROM users
			 WHERE id = $1
			   FOR NO KEY UPDATE`, p.UserID).Scan(&current, &status); err != nil {
			return mutation.Result{}, err
		}
		if err := mutation.CheckVersion(prep.ExpectedVersion, current); err != nil {
			return mutation.Result{}, err
		}
		if status != "active" {
			return mutation.Result{}, auth.ErrAccountLocked
		}

		deviceIDs, err := lockUserDevices(ctx, tx, p.UserID)
		if err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sessions
			   SET revoked_at = now(), updated_at = now()
			 WHERE user_id = $1 AND revoked_at IS NULL`, p.UserID); err != nil {
			return mutation.Result{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE devices
			   SET push_token_ciphertext = NULL,
			       push_environment = NULL,
			       revoked_at = COALESCE(revoked_at, now()),
			       version = version + 1,
			       updated_at = now()
			 WHERE user_id = $1`, p.UserID); err != nil {
			return mutation.Result{}, err
		}

		var version int64
		if err := tx.QueryRow(ctx, `
			UPDATE users
			   SET status = 'deletion_requested',
			       deletion_requested_at = now(),
			       version = version + 1,
			       updated_at = now()
			 WHERE id = $1
			RETURNING version`, p.UserID).Scan(&version); err != nil {
			return mutation.Result{}, err
		}
		if err := emitProjection(ctx, tx, p.UserID, func() (Projection, error) {
			return UserProjection(ctx, tx, p.UserID)
		}); err != nil {
			return mutation.Result{}, err
		}
		for _, id := range deviceIDs {
			deviceID := id
			if err := emitProjection(ctx, tx, p.UserID, func() (Projection, error) {
				return DeviceProjection(ctx, tx, deviceID)
			}); err != nil {
				return mutation.Result{}, err
			}
		}
		dedupe := p.UserID.String()
		if err := tx.Enqueue(ctx, mutation.Job{
			Type:      AccountDeletionJobType,
			Payload:   accountDeletionPayload{UserID: p.UserID.String()},
			DedupeKey: &dedupe,
		}); err != nil {
			return mutation.Result{}, err
		}
		if err := audit.Record(ctx, tx, audit.Event{Actor: &p.UserID, Action: "account.delete_request",
			TargetType: entityUser, TargetID: &p.UserID, Result: "success",
			RequestID: httpapi.RequestID(ctx)}); err != nil {
			return mutation.Result{}, err
		}
		return mutation.Result{StatusCode: http.StatusAccepted, ResourceType: entityUser, ResourceID: p.UserID, ResourceVersion: version}, nil
	})
	if !ok {
		return
	}
	h.writeMe(w, r, out.ResourceID, out.StatusCode)
}

func lockUserDevices(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT id FROM devices
		 WHERE user_id = $1
		 ORDER BY created_at, id
		   FOR NO KEY UPDATE`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// Opener는 저장된 Apple refresh token 암호문을 복호화한다. secretbox가 구현한다.
type Opener interface {
	Open(sealed []byte, purpose string) ([]byte, error)
}

// AppleRevoker는 Apple provider refresh token을 revoke한다. appleid.Client가 구현한다.
type AppleRevoker interface {
	Revoke(ctx context.Context, refreshToken string) error
}

// DeletionService는 account_deletion job을 처리한다.
type DeletionService struct {
	pool   *pgxpool.Pool
	opener Opener
	apple  AppleRevoker
}

type DeletionDeps struct {
	Pool   *pgxpool.Pool
	Opener Opener
	Apple  AppleRevoker
}

// NewDeletionService는 삭제 worker 서비스를 만든다.
func NewDeletionService(d DeletionDeps) (*DeletionService, error) {
	if d.Pool == nil || d.Opener == nil {
		return nil, errors.New("account: 삭제 worker에는 Pool과 Opener가 필요하다")
	}
	return &DeletionService{pool: d.Pool, opener: d.Opener, apple: d.Apple}, nil
}

// JobKind는 jobs.Runner에 등록할 account_deletion kind다.
func (s *DeletionService) JobKind() jobs.Kind {
	return jobs.Kind{Handler: s.HandleJob}
}

func (s *DeletionService) HandleJob(ctx context.Context, j jobs.Job) error {
	userID, err := parseDeletionPayload(j.Payload)
	if err != nil {
		return jobs.Permanent(err)
	}

	token, err := s.markDeleting(ctx, userID)
	if err != nil {
		return err
	}
	if token != nil {
		if s.apple == nil {
			return errors.New("account deletion: Apple revoker가 설정되지 않았다")
		}
		plain, err := s.opener.Open(token, auth.SealPurposeAppleRefreshToken)
		if err != nil {
			return fmt.Errorf("account deletion: Apple refresh token을 열 수 없다: %w", err)
		}
		if err := s.apple.Revoke(ctx, string(plain)); err != nil {
			if deletionRevokeAlreadyDone(err) {
				return s.finish(ctx, userID)
			}
			return fmt.Errorf("account deletion: Apple revoke가 실패했다: %w", err)
		}
	}
	return s.finish(ctx, userID)
}

func parseDeletionPayload(raw json.RawMessage) (uuid.UUID, error) {
	var p accountDeletionPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return uuid.Nil, fmt.Errorf("account deletion: payload가 올바르지 않다: %w", err)
	}
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return uuid.Nil, errors.New("account deletion: payload 뒤에 값이 더 있다")
	}
	id, err := uuid.Parse(p.UserID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("account deletion: user_id가 UUID가 아니다: %w", err)
	}
	return id, nil
}

func (s *DeletionService) markDeleting(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	var token []byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		var requestedAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT status, deletion_requested_at FROM users
			 WHERE id = $1
			   FOR NO KEY UPDATE`, userID).Scan(&status, &requestedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		switch status {
		case "deleted":
			return nil
		case "deletion_requested":
			if _, err := tx.Exec(ctx, `
				UPDATE users
				   SET status = 'deleting',
				       deleted_at = COALESCE(deleted_at, now()),
				       updated_at = now()
				 WHERE id = $1`, userID); err != nil {
				return err
			}
		case "deleting":
		default:
			return jobs.Permanent(fmt.Errorf("account deletion: 삭제 대상 상태가 아니다: %s", status))
		}
		if requestedAt == nil {
			return jobs.Permanent(errors.New("account deletion: 삭제 요청 시각이 없다"))
		}
		return tx.QueryRow(ctx, `
			SELECT provider_refresh_token_ciphertext
			  FROM auth_identities
			 WHERE user_id = $1 AND provider = 'apple'`, userID).Scan(&token)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return token, err
}

func (s *DeletionService) finish(ctx context.Context, userID uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM users
			 WHERE id = $1
			   FOR NO KEY UPDATE`, userID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) || status == "deleted" {
			return nil
		}
		if err != nil {
			return err
		}
		if status != "deleting" && status != "deletion_requested" {
			return jobs.Permanent(fmt.Errorf("account deletion: 완료 대상 상태가 아니다: %s", status))
		}
		if _, err := tx.Exec(ctx, `
			SELECT id FROM devices
			 WHERE user_id = $1
			 ORDER BY created_at, id
			   FOR NO KEY UPDATE`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sessions
			   SET revoked_at = now(), updated_at = now()
			 WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE devices
			   SET push_token_ciphertext = NULL,
			       push_environment = NULL,
			       revoked_at = COALESCE(revoked_at, now()),
			       version = version + 1,
			       updated_at = now()
			 WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM auth_identities
			 WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE audit_events
			   SET actor_tombstone = $2,
			       actor_user_id = NULL
			 WHERE actor_user_id = $1`, userID, auditTombstone(userID)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE users
			   SET display_name = '탈퇴한 사용자',
			       status = 'deleted',
			       deleted_at = COALESCE(deleted_at, now()),
			       updated_at = now()
			 WHERE id = $1`, userID)
		return err
	})
}

func deletionRevokeAlreadyDone(err error) bool {
	var apiErr *appleid.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "invalid_grant"
}

func auditTombstone(userID uuid.UUID) string {
	sum := sha256.Sum256([]byte("audit.actor.deleted_user:" + userID.String()))
	return "deleted_user:" + hex.EncodeToString(sum[:16])
}
