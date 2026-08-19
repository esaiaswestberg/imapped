package crypto_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/esaiaswestberg/imapped/internal/crypto"
)

func TestPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the hash contains the plaintext password")
	}

	ok, err := crypto.VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}

	ok, err = crypto.VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("an incorrect password verified")
	}
}

// Equal passwords must not produce equal hashes, or the database reveals which
// users share a password.
func TestPasswordHashesAreSalted(t *testing.T) {
	first, err := crypto.HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := crypto.HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if first == second {
		t.Error("identical passwords produced identical hashes, so they are unsalted")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, hash := range []string{
		"", "not-a-hash", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$abc$def",
		"$argon2id$v=19$m=65536,t=1,p=4$notbase64!$alsonot!",
	} {
		if _, err := crypto.VerifyPassword("anything", hash); !errors.Is(err, crypto.ErrInvalidHash) &&
			err == nil {
			t.Errorf("hash %q should have been rejected", hash)
		}
	}
}

func TestSealRoundTrip(t *testing.T) {
	sealer, err := crypto.NewSealer("a master key long enough to be plausible")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	const secret = "imap-app-password-1234"
	sealed, err := sealer.SealString(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("the ciphertext contains the plaintext")
	}

	opened, err := sealer.OpenString(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != secret {
		t.Errorf("round trip produced %q, want %q", opened, secret)
	}
}

// Nonce reuse under AES-GCM is catastrophic, so identical plaintexts must
// still encrypt to different ciphertexts.
func TestSealUsesAFreshNonce(t *testing.T) {
	sealer, err := crypto.NewSealer("a master key long enough to be plausible")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	first, err := sealer.SealString("identical")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := sealer.SealString("identical")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("identical plaintexts produced identical ciphertexts, so the nonce is reused")
	}
}

// A credential encrypted under one key must not open under another, and the
// failure must be explicit rather than yielding garbage.
func TestOpenWithWrongKeyFails(t *testing.T) {
	original, err := crypto.NewSealer("the original master key, suitably long")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	other, err := crypto.NewSealer("a different master key, also suitably long")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	sealed, err := original.SealString("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := other.OpenString(sealed); err == nil {
		t.Error("decryption succeeded under the wrong key")
	}
}

// Tampering must be detected, not silently accepted.
func TestOpenDetectsTampering(t *testing.T) {
	sealer, err := crypto.NewSealer("a master key long enough to be plausible")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	sealed, err := sealer.SealString("secret value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := sealer.Open(tampered); err == nil {
		t.Error("a modified ciphertext was accepted")
	}

	truncated := sealed[:len(sealed)-1]
	if _, err := sealer.Open(truncated); err == nil {
		t.Error("a truncated ciphertext was accepted")
	}

	if _, err := sealer.Open([]byte("tiny")); err == nil {
		t.Error("a ciphertext too short to hold a nonce was accepted")
	}
}

func TestSealerRequiresAKey(t *testing.T) {
	if _, err := crypto.NewSealer(""); !errors.Is(err, crypto.ErrNoKey) {
		t.Errorf("got %v, want ErrNoKey", err)
	}
}
