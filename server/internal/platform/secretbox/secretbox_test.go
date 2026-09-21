package secretbox

import (
	"bytes"
	"errors"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{7}, KeySize) }

// 로컬 구현이 배포 환경에서 조용히 동작하면 평문 키로 "암호화됐다"고 믿게 된다.
func TestNewLocalRefusesNonLocalEnv(t *testing.T) {
	for _, env := range []string{"staging", "production", "", "LOCAL"} {
		if _, err := NewLocal(env, testKey()); !errors.Is(err, ErrNotLocal) {
			t.Errorf("env=%q: err = %v, want ErrNotLocal", env, err)
		}
	}
}

func TestNewLocalRejectsWrongKeySize(t *testing.T) {
	if _, err := NewLocal("local", make([]byte, 16)); err == nil {
		t.Fatal("16바이트 키를 받아들였다")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := NewLocal("local", testKey())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal([]byte("apple-refresh-token"), "purpose-a")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("apple-refresh-token")) {
		t.Fatal("암호문에 평문이 그대로 들어 있다")
	}
	pt, err := b.Open(sealed, "purpose-a")
	if err != nil || string(pt) != "apple-refresh-token" {
		t.Fatalf("복호화 결과 %q, err=%v", pt, err)
	}

	again, _ := b.Seal([]byte("apple-refresh-token"), "purpose-a")
	if bytes.Equal(sealed, again) {
		t.Error("같은 평문이 같은 암호문이 됐다. nonce가 재사용된다")
	}
}

func TestOpenRejectsTamperingAndPurposeSwap(t *testing.T) {
	b, _ := NewLocal("local", testKey())
	sealed, _ := b.Seal([]byte("secret"), "purpose-a")

	if _, err := b.Open(sealed, "purpose-b"); !errors.Is(err, ErrOpen) {
		t.Errorf("다른 purpose로 열렸다: err=%v", err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := b.Open(tampered, "purpose-a"); !errors.Is(err, ErrOpen) {
		t.Errorf("변조된 암호문이 열렸다: err=%v", err)
	}
	other, _ := NewLocal("local", bytes.Repeat([]byte{8}, KeySize))
	if _, err := other.Open(sealed, "purpose-a"); !errors.Is(err, ErrOpen) {
		t.Errorf("다른 키로 열렸다: err=%v", err)
	}
	if _, err := b.Open([]byte{1, 2}, "purpose-a"); !errors.Is(err, ErrOpen) {
		t.Errorf("짧은 입력이 열렸다: err=%v", err)
	}
}
