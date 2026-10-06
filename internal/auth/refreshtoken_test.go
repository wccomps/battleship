package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/wccomps/battleship/internal/seal"
)

// oldSealRefreshToken is how refresh tokens were sealed before auth used
// internal/seal; tokens in existing sessions look like this.
func oldSealRefreshToken(t *testing.T, secret, sessionID, token string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("refresh token v1"))
	block, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := gcm.Seal(nonce, nonce, []byte(token), []byte("refresh token v1\x00"+sessionID))
	return "v1:" + base64.RawURLEncoding.EncodeToString(sealed)
}

func TestRefreshTokenOpensOldSeal(t *testing.T) {
	s := &Service{refreshKey: seal.NewKey("client-secret", refreshTokenPurpose)}
	stored := oldSealRefreshToken(t, "client-secret", "sess-1", "rt-abc")
	got, err := s.openRefreshToken("sess-1", stored)
	if err != nil || got != "rt-abc" {
		t.Fatalf("open old seal = %q, %v; want rt-abc", got, err)
	}
	if _, err := s.openRefreshToken("sess-2", stored); err == nil {
		t.Error("an old seal opened for another session")
	}
	if _, err := s.openRefreshToken("sess-1", oldSealRefreshToken(t, "client-secret", "sess-1", "")); err == nil {
		t.Error("an empty refresh token opened")
	}
	again, err := s.openRefreshToken("sess-1", s.sealRefreshToken("sess-1", "rt-abc"))
	if err != nil || again != "rt-abc" {
		t.Fatalf("round trip = %q, %v", again, err)
	}
}
