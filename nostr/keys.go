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

	var sk gonostr.SecretKey
	if strings.HasPrefix(raw, "nsec") {
		prefix, val, err := nip19.Decode(raw)
		if err != nil {
			return Keys{}, fmt.Errorf("decoding nsec: %w", err)
		}
		if prefix != "nsec" {
			return Keys{}, fmt.Errorf("expected nsec prefix, got %s", prefix)
		}
		var ok bool
		sk, ok = val.(gonostr.SecretKey)
		if !ok {
			return Keys{}, fmt.Errorf("nsec decoded to unexpected type %T", val)
		}
	} else {
		var err error
		sk, err = gonostr.SecretKeyFromHex(raw)
		if err != nil {
			return Keys{}, fmt.Errorf("parsing hex secret key: %w", err)
		}
	}

	pk := sk.Public()

	return Keys{SK: sk, PK: pk}, nil
}
