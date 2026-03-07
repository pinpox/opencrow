package nostr

import (
	"errors"
	"fmt"
	"strings"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// Keys holds a Nostr key pair.
type Keys struct {
	SK gonostr.SecretKey // secret key
	PK gonostr.PubKey    // public key
}

// loadKeys parses a private key (hex or nsec) and derives the public key.
func loadKeys(raw string) (Keys, error) {
	if raw == "" {
		return Keys{}, errors.New("empty private key")
	}

	sk, err := parseSecretKey(raw)
	if err != nil {
		return Keys{}, err
	}

	return Keys{SK: sk, PK: sk.Public()}, nil
}

func parseSecretKey(raw string) (gonostr.SecretKey, error) {
	if !strings.HasPrefix(raw, "nsec") {
		sk, err := gonostr.SecretKeyFromHex(raw)
		if err != nil {
			return gonostr.SecretKey{}, fmt.Errorf("parsing hex secret key: %w", err)
		}

		return sk, nil
	}

	prefix, val, err := nip19.Decode(raw)
	if err != nil {
		return gonostr.SecretKey{}, fmt.Errorf("decoding nsec: %w", err)
	}

	if prefix != "nsec" {
		return gonostr.SecretKey{}, fmt.Errorf("expected nsec prefix, got %s", prefix)
	}

	sk, ok := val.(gonostr.SecretKey)
	if !ok {
		return gonostr.SecretKey{}, fmt.Errorf("nsec decoded to unexpected type %T", val)
	}

	return sk, nil
}

// DecodeNsecToHex decodes an nsec-encoded private key to its hex
// representation. If the input is already hex, it is returned as-is.
func DecodeNsecToHex(raw string) (string, error) {
	sk, err := parseSecretKey(raw)
	if err != nil {
		return "", err
	}

	return sk.Hex(), nil
}

// DecodeNpubToHex decodes an npub-encoded public key to its hex
// representation. If the input is already hex, it is validated and
// returned. Returns an error for invalid input.
func DecodeNpubToHex(raw string) (string, error) {
	if !strings.HasPrefix(raw, "npub") {
		if _, err := gonostr.PubKeyFromHex(raw); err != nil {
			return "", fmt.Errorf("parsing hex public key: %w", err)
		}

		return raw, nil
	}

	prefix, val, err := nip19.Decode(raw)
	if err != nil {
		return "", fmt.Errorf("decoding npub %q: %w", raw, err)
	}

	if prefix != "npub" {
		return "", fmt.Errorf("expected npub prefix, got %s", prefix)
	}

	pk, ok := val.(gonostr.PubKey)
	if !ok {
		return "", fmt.Errorf("npub decoded to unexpected type %T", val)
	}

	return pk.Hex(), nil
}
