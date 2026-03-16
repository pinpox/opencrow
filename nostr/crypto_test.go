package nostr

import (
	"encoding/hex"
	"testing"
)

func TestEncryptFileForUpload_RoundTrip(t *testing.T) {
	t.Parallel()

	plaintext := []byte("confidential file content 🔒")

	enc, err := encryptFileForUpload(plaintext)
	if err != nil {
		t.Fatalf("encryptFileForUpload: %v", err)
	}

	// Ciphertext must differ from plaintext.
	if string(enc.Ciphertext) == string(plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	// Decrypt and verify round-trip.
	key, err := hex.DecodeString(enc.KeyHex)
	if err != nil {
		t.Fatalf("decoding key: %v", err)
	}

	nonce, err := hex.DecodeString(enc.NonceHex)
	if err != nil {
		t.Fatalf("decoding nonce: %v", err)
	}

	got, err := decryptAESGCM(key, nonce, enc.Ciphertext)
	if err != nil {
		t.Fatalf("decryptAESGCM: %v", err)
	}

	if string(got) != string(plaintext) {
		t.Errorf("decrypted = %q, want %q", got, plaintext)
	}
}
