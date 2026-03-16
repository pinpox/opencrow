package nostr

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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
// It accepts 16- or 32-byte keys (AES-128/256) and does not validate nonce
// length here; decryptAESGCM uses NewGCMWithNonceSize to tolerate non-standard
// nonce lengths that some Nostr clients send in the wild.
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

	if len(key) != 16 && len(key) != 32 {
		return nil, nil, fmt.Errorf("decryption key must be 16 or 32 bytes (AES-128/256), got %d", len(key))
	}

	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding decryption nonce: %w", err)
	}

	return key, nonce, nil
}

// decryptAESGCM decrypts ciphertext using AES-GCM with the given key and
// nonce. Uses NewGCMWithNonceSize to accept non-standard nonce lengths
// that some Nostr clients send in the wild.
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

// encryptedFile holds the result of encrypting a file for Blossom upload.
type encryptedFile struct {
	Ciphertext []byte // AES-GCM encrypted content
	KeyHex     string // hex-encoded 256-bit AES key
	NonceHex   string // hex-encoded 12-byte GCM nonce
	OxHex      string // hex-encoded SHA-256 of the plaintext (pre-encryption hash)
}

// encryptFileForUpload encrypts plaintext with a fresh AES-256-GCM key and
// nonce. Returns the ciphertext and all parameters needed for the recipient
// to decrypt (transmitted inside the encrypted kind 15 rumor tags).
func encryptFileForUpload(plaintext []byte) (*encryptedFile, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating AES key: %w", err)
	}

	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	oxHash := sha256.Sum256(plaintext)

	return &encryptedFile{
		Ciphertext: ciphertext,
		KeyHex:     hex.EncodeToString(key),
		NonceHex:   hex.EncodeToString(nonce),
		OxHex:      hex.EncodeToString(oxHash[:]),
	}, nil
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
