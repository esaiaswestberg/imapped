package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ErrNoKey is returned when sealing is attempted without a master key.
var ErrNoKey = errors.New("no encryption master key configured")

// Sealer encrypts upstream credentials so that a database dump alone does not
// reveal them: recovering a password requires both the dump and the master key,
// which lives in configuration rather than in the database.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer derives an encryption key from the configured master key.
//
// HKDF rather than using the master key directly: the configured value is an
// arbitrary string of unknown entropy distribution, and HKDF turns it into a
// uniformly distributed key of exactly the right length. The info parameter
// domain-separates this use, so the same master key can safely derive other
// keys for other purposes later.
func NewSealer(masterKey string) (*Sealer, error) {
	if masterKey == "" {
		return nil, ErrNoKey
	}

	key := make([]byte, 32)
	reader := hkdf.New(sha256.New, []byte(masterKey), nil, []byte("imapped upstream credentials v1"))
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("deriving encryption key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating AEAD: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext, returning nonce-prefixed ciphertext.
//
// A fresh random nonce per call is essential: reusing one under the same key
// with GCM is catastrophic, revealing the XOR of the two plaintexts and
// allowing forgery.
func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// SealString is Seal for a string value.
func (s *Sealer) SealString(plaintext string) ([]byte, error) {
	return s.Seal([]byte(plaintext))
}

// Open decrypts ciphertext produced by Seal.
//
// Authentication failure means the data was tampered with or the master key
// changed; either way the credential is unusable and must not be guessed at.
func (s *Sealer) Open(ciphertext []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext is too short to contain a nonce")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]

	plaintext, err := s.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypting credential (wrong master key, or the "+
			"stored value was corrupted): %w", err)
	}
	return plaintext, nil
}

// OpenString is Open returning a string.
func (s *Sealer) OpenString(ciphertext []byte) (string, error) {
	plaintext, err := s.Open(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
