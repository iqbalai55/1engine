package tenant

import (
	"bytes"
	"strings"
	"testing"
)

// credentialKey builds a distinct 32 byte encryption key from a seed.
func credentialKey(seed byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func TestEncryptCredentialRoundTripsPlaintext(t *testing.T) {
	cases := []struct {
		name      string
		plaintext string
	}{
		{name: "customer token", plaintext: "dstack-customer-secret"},
		{name: "token with punctuation", plaintext: "tok_2f9!@#$%^&*() value"},
		{name: "multiline token", plaintext: "first-line\nsecond-line\n"},
		{name: "non ascii token", plaintext: "clé-dstack-秘密"},
		{name: "long token", plaintext: strings.Repeat("long-secret-", 64)},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ciphertext, err := EncryptCredential(credentialKey(1), test.plaintext)
			if err != nil {
				t.Fatalf("EncryptCredential() = %v, want a ciphertext", err)
			}
			if len(ciphertext) == 0 {
				t.Fatal("EncryptCredential() returned an empty ciphertext")
			}
			if bytes.Contains(ciphertext, []byte(test.plaintext)) {
				t.Fatal("EncryptCredential() left the plaintext readable in the ciphertext")
			}
			got, err := DecryptCredential(credentialKey(1), ciphertext)
			if err != nil {
				t.Fatalf("DecryptCredential() = %v, want the plaintext back", err)
			}
			if got != test.plaintext {
				t.Fatalf("DecryptCredential() = %q, want %q", got, test.plaintext)
			}
		})
	}
}

func TestEncryptCredentialProducesUniqueCiphertexts(t *testing.T) {
	key := credentialKey(2)
	plaintext := "dstack-customer-secret"
	seen := make(map[string]bool, 32)
	for i := 0; i < 32; i++ {
		ciphertext, err := EncryptCredential(key, plaintext)
		if err != nil {
			t.Fatalf("EncryptCredential() = %v, want a ciphertext", err)
		}
		if seen[string(ciphertext)] {
			t.Fatal("EncryptCredential() repeated a ciphertext for the same plaintext and key")
		}
		seen[string(ciphertext)] = true
		got, err := DecryptCredential(key, ciphertext)
		if err != nil {
			t.Fatalf("DecryptCredential() = %v, want the plaintext back", err)
		}
		if got != plaintext {
			t.Fatalf("DecryptCredential() = %q, want %q", got, plaintext)
		}
	}
}

func TestDecryptCredentialRejectsWrongKey(t *testing.T) {
	ciphertext, err := EncryptCredential(credentialKey(3), "dstack-customer-secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptCredential(credentialKey(4), ciphertext)
	if err == nil {
		t.Fatal("DecryptCredential() accepted a ciphertext under the wrong key")
	}
	if got != "" {
		t.Fatalf("DecryptCredential() = %q, want no plaintext on failure", got)
	}
}

func TestDecryptCredentialRejectsTamperedCiphertext(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ciphertext []byte) []byte
	}{
		{
			name: "flipped first byte",
			mutate: func(ciphertext []byte) []byte {
				tampered := bytes.Clone(ciphertext)
				tampered[0] ^= 0x01
				return tampered
			},
		},
		{
			name: "flipped middle byte",
			mutate: func(ciphertext []byte) []byte {
				tampered := bytes.Clone(ciphertext)
				tampered[len(tampered)/2] ^= 0x01
				return tampered
			},
		},
		{
			name: "flipped last byte",
			mutate: func(ciphertext []byte) []byte {
				tampered := bytes.Clone(ciphertext)
				tampered[len(tampered)-1] ^= 0x01
				return tampered
			},
		},
		{
			name: "truncated final byte",
			mutate: func(ciphertext []byte) []byte {
				return ciphertext[:len(ciphertext)-1]
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ciphertext, err := EncryptCredential(credentialKey(5), "dstack-customer-secret")
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecryptCredential(credentialKey(5), test.mutate(ciphertext))
			if err == nil {
				t.Fatal("DecryptCredential() accepted a tampered ciphertext")
			}
			if got != "" {
				t.Fatalf("DecryptCredential() = %q, want no plaintext on failure", got)
			}
		})
	}
}

func TestDecryptCredentialRejectsShortCiphertext(t *testing.T) {
	cases := []struct {
		name       string
		ciphertext []byte
	}{
		{name: "empty", ciphertext: []byte{}},
		{name: "one byte", ciphertext: []byte{0x01}},
		{name: "eight bytes", ciphertext: make([]byte, 8)},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecryptCredential(credentialKey(6), test.ciphertext)
			if err == nil {
				t.Fatal("DecryptCredential() accepted a short ciphertext")
			}
			if !strings.Contains(err.Error(), "invalid encrypted dstack credential") {
				t.Fatalf("DecryptCredential() error = %q, want it to report an invalid credential", err)
			}
			if got != "" {
				t.Fatalf("DecryptCredential() = %q, want no plaintext on failure", got)
			}
		})
	}
}

func TestCredentialEncryptionRejectsInvalidKeyLength(t *testing.T) {
	cases := []struct {
		name string
		key  []byte
	}{
		{name: "empty key", key: []byte{}},
		{name: "short key", key: make([]byte, 16)},
		{name: "one byte short", key: make([]byte, 31)},
		{name: "one byte long", key: make([]byte, 33)},
		{name: "long key", key: make([]byte, 64)},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ciphertext, err := EncryptCredential(test.key, "dstack-customer-secret")
			if err == nil {
				t.Fatal("EncryptCredential() accepted a key that is not 32 bytes")
			}
			if !strings.Contains(err.Error(), "must be 32 bytes") {
				t.Fatalf("EncryptCredential() error = %q, want it to report the key length", err)
			}
			if ciphertext != nil {
				t.Fatalf("EncryptCredential() = %x, want no ciphertext on failure", ciphertext)
			}
			got, err := DecryptCredential(test.key, make([]byte, 64))
			if err == nil {
				t.Fatal("DecryptCredential() accepted a key that is not 32 bytes")
			}
			if !strings.Contains(err.Error(), "must be 32 bytes") {
				t.Fatalf("DecryptCredential() error = %q, want it to report the key length", err)
			}
			if got != "" {
				t.Fatalf("DecryptCredential() = %q, want no plaintext on failure", got)
			}
		})
	}
}
