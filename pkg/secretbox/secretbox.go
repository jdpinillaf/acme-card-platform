// Package secretbox seals small secrets (destination bank account numbers) with AES-256-GCM.
// The key comes from configuration (KMS in AWS, a dev key locally); a fresh random nonce is
// prepended to every ciphertext.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// Box seals and opens values with one key.
type Box struct{ aead cipher.AEAD }

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secretbox: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plain and returns base64(nonce ‖ ciphertext).
func (b *Box) Seal(plain []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, plain, nil)), nil
}

// Open reverses Seal; tampering or a wrong key is an error.
func (b *Box) Open(sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	return b.aead.Open(nil, raw[:n], raw[n:], nil)
}

// Last4 is the masked tail shown everywhere outside the bank instruction.
func Last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
