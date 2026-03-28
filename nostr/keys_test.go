package nostr

import (
	"testing"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

func TestLoadKeys(t *testing.T) {
	t.Parallel()

	sk := gonostr.Generate()
	wantPK := sk.Public()

	cases := []struct {
		name  string
		input string
	}{
		{"hex", sk.Hex()},
		{"nsec", nip19.EncodeNsec(sk)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			keys, err := loadKeys(tc.input)
			if err != nil {
				t.Fatalf("loadKeys: %v", err)
			}

			if keys.SK != sk {
				t.Errorf("SK = %s, want %s", keys.SK, sk)
			}

			if keys.PK != wantPK {
				t.Errorf("PK = %s, want %s", keys.PK, wantPK)
			}
		})
	}
}

func TestDecodeNpubToHex_InvalidHex(t *testing.T) {
	t.Parallel()

	_, err := DecodeNpubToHex("not-valid-hex")
	if err == nil {
		t.Fatal("expected error for invalid hex input, got nil")
	}
}
