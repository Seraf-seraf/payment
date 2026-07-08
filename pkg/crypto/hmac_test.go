package crypto

import "testing"

func TestEncryptDecryptSecret(t *testing.T) {
	t.Parallel()

	encrypted, err := EncryptSecret("master-key", "shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	if encrypted == "shared-secret" {
		t.Fatal("secret was not encrypted")
	}
	decrypted, err := DecryptSecret("master-key", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != "shared-secret" {
		t.Fatalf("decrypted = %q", decrypted)
	}
}

func TestDecryptSecretAllowsPlaintextForLocalCompatibility(t *testing.T) {
	t.Parallel()

	decrypted, err := DecryptSecret("", "plain-secret")
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != "plain-secret" {
		t.Fatalf("decrypted = %q", decrypted)
	}
}
