// Package seal encrypts secrets kept at rest (Proxmox tickets and tokens)
// with AES-256-GCM, bound to the record that holds them, so a database leak
// (a backup, a replica, a stray dump) doesn't hand them out, and a sealed
// value copied into another record doesn't open.
//
// Stored format: "v1:" followed by the unpadded base64url of nonce ||
// ciphertext. Values without the prefix are never accepted.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const version = "v1:"

// ErrOpen means a sealed value didn't open: it is damaged, belongs to
// another record, or was sealed with another key (the secret changed).
var ErrOpen = errors.New("the sealed value can't be opened (was the key changed?)")

// Key seals values for one purpose.
type Key struct {
	key     []byte
	purpose string
}

// NewKey derives a 32-byte key for purpose from secret, so each use of one
// secret has its own key.
func NewKey(secret, purpose string) Key {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(purpose))
	return Key{key: m.Sum(nil), purpose: purpose}
}

func (k Key) gcm() cipher.AEAD {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		panic(err) // the key is always 32 bytes
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
}

func (k Key) aad(binding string) []byte { return []byte(k.purpose + "\x00" + binding) }

// Seal encrypts plain for the record named by binding, e.g. "job:7".
func (k Key) Seal(binding string, plain []byte) string {
	g := k.gcm()
	nonce := make([]byte, g.NonceSize())
	_, _ = rand.Read(nonce) // never fails
	return version + base64.RawURLEncoding.EncodeToString(g.Seal(nonce, nonce, plain, k.aad(binding)))
}

// Open reverses Seal for the same binding.
func (k Key) Open(binding, stored string) ([]byte, error) {
	body, ok := strings.CutPrefix(stored, version)
	if !ok {
		return nil, ErrOpen
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	g := k.gcm()
	if err != nil || len(raw) < g.NonceSize()+g.Overhead() {
		return nil, ErrOpen
	}
	plain, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], k.aad(binding))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// String, GoString and Format never show the key's bytes.
func (k Key) String() string   { return "seal.Key{" + k.purpose + ", [redacted]}" }
func (k Key) GoString() string { return k.String() }

// Format makes every verb print String.
func (k Key) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }
