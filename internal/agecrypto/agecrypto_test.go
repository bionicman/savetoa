package agecrypto

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestEncryptReaderRoundTrip(t *testing.T) {
	identity, encryptor := testEncryptor(t)
	plaintext := "backup payload"
	ciphertextReader, err := encryptor.EncryptReader(context.Background(), strings.NewReader(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := io.ReadAll(ciphertextReader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}
	decrypted, err := age.Decrypt(strings.NewReader(string(ciphertext)), identity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(decrypted)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plaintext {
		t.Fatalf("decrypted payload = %q, want %q", got, plaintext)
	}
	transformation := encryptor.Transformation()
	if transformation.Driver != "age" || len(transformation.RecipientFingerprints) != 1 ||
		!strings.HasPrefix(transformation.RecipientFingerprints[0], "sha256:") {
		t.Fatalf("Transformation() = %#v", transformation)
	}
}

func TestParseRecipientsAcceptsCommentsAndCRLF(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	data := "# escrow recipient\r\n\r\n" + identity.Recipient().String() + "\r\n"
	encryptor, err := ParseRecipients(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(encryptor.Transformation().RecipientFingerprints) != 1 {
		t.Fatal("recipient was not parsed")
	}
}

func TestParseRecipientsRejectsSecretsWithoutEchoingThem(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	secret := identity.String()
	_, err = ParseRecipients(strings.NewReader(secret + "\n"))
	if err == nil || !strings.Contains(err.Error(), "only public") {
		t.Fatalf("ParseRecipients(identity) error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error contains age identity")
	}
	invalid := "credential-shaped-but-invalid-value"
	_, err = ParseRecipients(strings.NewReader(invalid + "\n"))
	if err == nil {
		t.Fatal("ParseRecipients(invalid) error = nil")
	}
	if strings.Contains(err.Error(), invalid) {
		t.Fatal("error contains invalid recipient input")
	}
}

func TestParseRecipientsFailsClosed(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := identity.Recipient().String()
	tests := map[string]struct {
		data string
		want string
	}{
		"empty":     {data: "# no recipients\n", want: "contains no public"},
		"invalid":   {data: "not-a-recipient\n", want: "invalid public X25519"},
		"duplicate": {data: recipient + "\n" + recipient + "\n", want: "duplicate"},
		"oversized": {data: strings.Repeat("x", MaxRecipientsFileSize+1), want: "exceeds"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRecipients(strings.NewReader(test.data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseRecipients() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEncryptReaderHonorsCancellation(t *testing.T) {
	_, encryptor := testEncryptor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := encryptor.EncryptReader(ctx, strings.NewReader("payload"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EncryptReader(cancelled) error = %v", err)
	}
}

func TestParseIdentitiesAndDecryptReader(t *testing.T) {
	identity, encryptor := testEncryptor(t)
	ciphertext, err := encryptor.EncryptReader(context.Background(), strings.NewReader("database pages"))
	if err != nil {
		t.Fatal(err)
	}
	identities, err := ParseIdentities(strings.NewReader("# restore escrow\n" + identity.String() + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := DecryptReader(context.Background(), ciphertext, identities)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(plaintext)
	if err != nil || string(data) != "database pages" {
		t.Fatalf("decrypted data = %q, error = %v", data, err)
	}
}

func TestParseIdentitiesDoesNotEchoInvalidInput(t *testing.T) {
	secretShaped := "AGE-SECRET-KEY-PRIVATE-MATERIAL"
	_, err := ParseIdentities(strings.NewReader(secretShaped))
	if err == nil {
		t.Fatal("ParseIdentities(invalid) error = nil")
	}
	if strings.Contains(err.Error(), secretShaped) {
		t.Fatal("identity parser error contains input")
	}
}

func testEncryptor(t *testing.T) (*age.X25519Identity, *Encryptor) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	encryptor, err := ParseRecipients(strings.NewReader(identity.Recipient().String() + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return identity, encryptor
}
