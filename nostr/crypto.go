package nostr

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	gonostr "fiatjaf.com/nostr"
)

// decryptFileInPlace reads the file at path, decrypts it using AES-GCM with
// the key and nonce from the rumor tags, verifies the SHA-256 hash against
// the "ox" tag (pre-encryption hash), and writes the plaintext back.
func decryptFileInPlace(filePath string, tags gonostr.Tags) error {
	algo := tagValue(tags, "encryption-algorithm")
	if algo != "aes-gcm" {
		return fmt.Errorf("unsupported encryption algorithm: %q", algo)
	}

	key, nonce, err := parseDecryptionParams(tags)
	if err != nil {
		return err
	}

	ciphertext, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("reading encrypted file: %w", err)
	}

	plaintext, err := decryptAESGCM(key, nonce, ciphertext)
	if err != nil {
		return err
	}

	// Verify against the pre-encryption hash if provided.
	if oxHex := tagValue(tags, "ox"); oxHex != "" {
		hash := sha256.Sum256(plaintext)

		if hex.EncodeToString(hash[:]) != oxHex {
			return errors.New("SHA-256 mismatch after decryption")
		}
	}

	if err := os.WriteFile(filePath, plaintext, 0o600); err != nil {
		return fmt.Errorf("writing decrypted file: %w", err)
	}

	return nil
}

// parseDecryptionParams extracts and decodes the AES key and nonce from tags.
func parseDecryptionParams(tags gonostr.Tags) ([]byte, []byte, error) {
	keyHex := tagValue(tags, "decryption-key")
	nonceHex := tagValue(tags, "decryption-nonce")

	if keyHex == "" || nonceHex == "" {
		return nil, nil, errors.New("missing decryption-key or decryption-nonce tags")
	}

	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding decryption key: %w", err)
	}

	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding decryption nonce: %w", err)
	}

	return key, nonce, nil
}

// decryptAESGCM decrypts ciphertext using AES-GCM with the given key and nonce.
func decryptAESGCM(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM decryption: %w", err)
	}

	return plaintext, nil
}

// tagValue returns the value of the first tag with the given key, or "".
func tagValue(tags gonostr.Tags, key string) string {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == key {
			return tag[1]
		}
	}

	return ""
}
