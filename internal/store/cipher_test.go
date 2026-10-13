package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSensitiveFieldsEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{
		ID:       "acc-test",
		Email:    "test@example.com",
		Password: "password-secret",
		Credentials: &Credentials{
			AccessKeyID:     "AK-test",
			SecretAccessKey: "SK-secret",
			SecurityToken:   "token-secret",
			Expiration:      time.Now().Add(time.Hour),
		},
	}
	if err := db.Upsert(account); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dir + string(os.PathSeparator) + "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"password-secret", "SK-secret", "token-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("plaintext secret %q found in accounts.json", secret)
		}
	}
	if !strings.Contains(string(data), "enc:") {
		t.Fatal("encrypted marker not found in accounts.json")
	}

	db2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := db2.Get("acc-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Password != "password-secret" || loaded.Credentials.SecretAccessKey != "SK-secret" || loaded.Credentials.SecurityToken != "token-secret" {
		t.Fatalf("decrypted values mismatch: %#v", loaded)
	}
	_ = db2.Save()
}
