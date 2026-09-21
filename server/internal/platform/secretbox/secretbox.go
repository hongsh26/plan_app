// Package secretbox는 DB에 저장할 민감 값의 암호화 경계다.
//
// account_backend_design.md §11은 Apple provider refresh token과 푸시 token을
// KMS 기반 envelope encryption으로 저장하라고 한다. 로컬에는 KMS가 없으므로
// 이 패키지는 로컬 전용 구현(LocalAESGCM) 하나만 제공한다.
//
// LocalAESGCM은 APP_ENV=local이 아니면 만들어지지 않는다. 로컬 구현이 스테이징·
// 프로덕션에서 조용히 동작하면 키가 환경 변수에 평문으로 놓인 채 "암호화됐다"고
// 믿게 되므로, 그보다는 기동이 실패하는 편이 낫다. 배포 환경의 KMS 구현은
// 별도 작업이다(docs/progressing.md).
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"plantogether/server/internal/platform/config"
)

// KeySize는 LocalAESGCM 키 길이(AES-256)다.
const KeySize = 32

// localV1은 암호문 앞에 붙는 형식 표시다. KMS 구현이 들어오면 다른 값을 써서
// 저장된 암호문이 어느 구현으로 만들어졌는지 구분하고 재암호화할 수 있게 한다.
const localV1 byte = 0x01

var (
	// ErrNotLocal은 로컬이 아닌 환경에서 LocalAESGCM을 만들려 할 때다.
	ErrNotLocal = errors.New("secretbox: 로컬 암호화 구현은 APP_ENV=local에서만 쓸 수 있다. 배포 환경은 KMS 구현이 필요하다")

	// ErrOpen은 복호화 실패다. 키가 다르거나, 암호문이 변조됐거나, 다른
	// 용도(associated data)로 봉인된 값이다. 원인을 구분하지 않는다.
	ErrOpen = errors.New("secretbox: 복호화할 수 없다")
)

// LocalAESGCM은 AES-256-GCM으로 봉인한다. 로컬 개발 전용이다.
type LocalAESGCM struct {
	aead cipher.AEAD
}

// NewLocal은 LocalAESGCM을 만든다. env가 local이 아니면 ErrNotLocal이다.
func NewLocal(env string, key []byte) (*LocalAESGCM, error) {
	if env != config.EnvLocal {
		return nil, ErrNotLocal
	}
	if len(key) != KeySize {
		return nil, errors.New("secretbox: 키는 32바이트여야 한다")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("secretbox: 키로 AES를 초기화할 수 없다")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("secretbox: GCM을 초기화할 수 없다")
	}
	return &LocalAESGCM{aead: aead}, nil
}

// Seal은 plaintext를 봉인한다. purpose는 associated data다. 같은 키로 봉인한
// 값이라도 다른 purpose로는 열리지 않아, 한 열의 암호문을 다른 열에 옮겨 넣는
// 공격을 막는다.
//
// 형식: [localV1][nonce 12바이트][암호문+태그]
func (b *LocalAESGCM) Seal(plaintext []byte, purpose string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("secretbox: nonce를 만들 수 없다")
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, localV1)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, []byte(purpose)), nil
}

// Open은 Seal의 역이다.
func (b *LocalAESGCM) Open(sealed []byte, purpose string) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < 1+ns+b.aead.Overhead() || sealed[0] != localV1 {
		return nil, ErrOpen
	}
	nonce := sealed[1 : 1+ns]
	pt, err := b.aead.Open(nil, nonce, sealed[1+ns:], []byte(purpose))
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}
