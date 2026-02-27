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
