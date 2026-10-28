package party

import (
	"fmt"

	"github.com/google/uuid"
)

const (
	sealPurposeProjectionTitlePrefix    = "party_projection_title"
	sealPurposeProjectionLocationPrefix = "party_projection_location"
)

// SealPurposeProjectionTitle은 projection title_ciphertext의 associated data다.
func SealPurposeProjectionTitle(projectionID uuid.UUID) string {
	return projectionDetailsPurpose(sealPurposeProjectionTitlePrefix, projectionID)
}

// SealPurposeProjectionLocation은 projection location_ciphertext의 associated data다.
func SealPurposeProjectionLocation(projectionID uuid.UUID) string {
	return projectionDetailsPurpose(sealPurposeProjectionLocationPrefix, projectionID)
}

func projectionDetailsPurpose(prefix string, projectionID uuid.UUID) string {
	return fmt.Sprintf("%s:%s", prefix, projectionID.String())
}

// DetailBox는 snapshot 상세 allowlist를 projection ciphertext로 봉인한다.
type DetailBox interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

// ProjectionPlainDetails는 snapshot staging에서 Party projection으로 승격할 수 있는 상세 allowlist다.
// TitlePlaintext가 nil이어도 HasDetails가 true이면 빈 제목 allowlist를 받은 것으로 본다.
type ProjectionPlainDetails struct {
	HasDetails        bool
	TitlePlaintext    []byte
	LocationPlaintext []byte
}
