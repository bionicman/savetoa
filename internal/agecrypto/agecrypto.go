// Package agecrypto applies recipient-only age encryption to backup payloads.
package agecrypto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"github.com/bionicman/savetoa/internal/manifest"
)

const (
	MaxRecipientsFileSize = 64 << 10
	MaxIdentitiesFileSize = 64 << 10
	MaxRecipients         = 100
	maxRecipientLineSize  = 1024
)

func ParseIdentities(reader io.Reader) ([]age.Identity, error) {
	if reader == nil {
		return nil, errors.New("identity reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxIdentitiesFileSize+1))
	if err != nil {
		return nil, errors.New("read age identity file")
	}
	if len(data) > MaxIdentitiesFileSize {
		return nil, fmt.Errorf("age identity file exceeds %d bytes", MaxIdentitiesFileSize)
	}

	identities := make([]age.Identity, 0, 1)
	for _, rawLine := range bytes.Split(data, []byte{'\n'}) {
		line := strings.TrimSpace(strings.TrimSuffix(string(rawLine), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		identity, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, errors.New("age identity file contains an invalid X25519 identity")
		}
		identities = append(identities, identity)
	}
	if len(identities) == 0 {
		return nil, errors.New("age identity file contains no X25519 identities")
	}
	return identities, nil
}

func DecryptReader(ctx context.Context, ciphertext io.Reader, identities []age.Identity) (io.Reader, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if ciphertext == nil {
		return nil, errors.New("ciphertext reader is required")
	}
	if len(identities) == 0 {
		return nil, errors.New("at least one age identity is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("decrypt payload: %w", err)
	}
	plaintext, err := age.Decrypt(&contextReader{ctx: ctx, reader: ciphertext}, identities...)
	if err != nil {
		return nil, errors.New("decrypt age payload")
	}
	return plaintext, nil
}

type Encryptor struct {
	recipients   []age.Recipient
	fingerprints []string
}

func ParseRecipients(reader io.Reader) (*Encryptor, error) {
	if reader == nil {
		return nil, errors.New("recipient reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxRecipientsFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read age recipients: %w", err)
	}
	if len(data) > MaxRecipientsFileSize {
		return nil, fmt.Errorf("age recipients file exceeds %d bytes", MaxRecipientsFileSize)
	}

	var recipients []age.Recipient
	var fingerprints []string
	seen := make(map[string]struct{})
	for index, rawLine := range bytes.Split(data, []byte{'\n'}) {
		lineNumber := index + 1
		line := strings.TrimSpace(strings.TrimSuffix(string(rawLine), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > maxRecipientLineSize {
			return nil, fmt.Errorf("age recipient on line %d is too long", lineNumber)
		}
		if strings.Contains(line, "AGE-SECRET-KEY-") {
			return nil, fmt.Errorf("age identity found on line %d; only public X25519 recipients are allowed", lineNumber)
		}
		recipient, err := age.ParseX25519Recipient(line)
		if err != nil {
			return nil, fmt.Errorf("invalid public X25519 recipient on line %d", lineNumber)
		}
		canonical := recipient.String()
		if _, exists := seen[canonical]; exists {
			return nil, fmt.Errorf("duplicate age recipient on line %d", lineNumber)
		}
		if len(recipients) == MaxRecipients {
			return nil, fmt.Errorf("age recipients file contains more than %d recipients", MaxRecipients)
		}
		seen[canonical] = struct{}{}
		recipients = append(recipients, recipient)
		digest := sha256.Sum256([]byte(canonical))
		fingerprints = append(fingerprints, "sha256:"+hex.EncodeToString(digest[:]))
	}
	if len(recipients) == 0 {
		return nil, errors.New("age recipients file contains no public X25519 recipients")
	}
	return &Encryptor{recipients: recipients, fingerprints: fingerprints}, nil
}

func (encryptor *Encryptor) EncryptReader(ctx context.Context, plaintext io.Reader) (io.Reader, error) {
	if encryptor == nil || len(encryptor.recipients) == 0 {
		return nil, errors.New("age encryptor has no recipients")
	}
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if plaintext == nil {
		return nil, errors.New("plaintext reader is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("encrypt payload: %w", err)
	}
	reader, err := age.EncryptReader(&contextReader{ctx: ctx, reader: plaintext}, encryptor.recipients...)
	if err != nil {
		return nil, errors.New("initialize age encryption")
	}
	return reader, nil
}

func (encryptor *Encryptor) Transformation() manifest.Transformation {
	fingerprints := append([]string(nil), encryptor.fingerprints...)
	return manifest.Transformation{
		Driver:                "age",
		RecipientFingerprints: fingerprints,
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
