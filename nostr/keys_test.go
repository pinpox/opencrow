package nostr

import (
	"testing"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

func TestLoadKeys_Hex(t *testing.T) {
	sk := gonostr.Generate()
	wantPK := sk.Public()

	keys, err := loadKeys(sk.Hex())
	if err != nil {
		t.Fatalf("loadKeys: %v", err)
	}
	if keys.SK != sk {
		t.Errorf("SK = %s, want %s", keys.SK, sk)
	}
	if keys.PK != wantPK {
		t.Errorf("PK = %s, want %s", keys.PK, wantPK)
	}
}

func TestLoadKeys_Nsec(t *testing.T) {
	sk := gonostr.Generate()
	wantPK := sk.Public()
	nsec := nip19.EncodeNsec(sk)

	keys, err := loadKeys(nsec)
	if err != nil {
		t.Fatalf("loadKeys: %v", err)
	}
	if keys.SK != sk {
		t.Errorf("SK = %s, want %s", keys.SK, sk)
	}
	if keys.PK != wantPK {
		t.Errorf("PK = %s, want %s", keys.PK, wantPK)
	}
}
