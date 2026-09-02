package mongodb

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

const maxCredentialsSize = 64 << 10

type Credentials struct {
	Password string `yaml:"password"`
}

func ReadCredentialsFile(filename string) (Credentials, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return Credentials{}, errors.New("inspect MongoDB credentials file")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Credentials{}, errors.New("MongoDB credentials file must be a regular non-symlink file with mode 0600")
	}
	file, err := os.Open(filename)
	if err != nil {
		return Credentials{}, errors.New("open MongoDB credentials file")
	}
	defer file.Close()
	return ReadCredentials(file)
}

func ReadCredentials(reader io.Reader) (Credentials, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCredentialsSize+1))
	if err != nil {
		return Credentials{}, errors.New("read MongoDB credentials")
	}
	if len(data) > maxCredentialsSize {
		return Credentials{}, errors.New("MongoDB credentials file is too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var result Credentials
	if err := decoder.Decode(&result); err != nil {
		return Credentials{}, errors.New("decode MongoDB credentials")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Credentials{}, errors.New("MongoDB credentials must contain exactly one YAML document")
	}
	if result.Password == "" || strings.ContainsAny(result.Password, "\r\n\x00") {
		return Credentials{}, errors.New("MongoDB password must be a non-empty single-line value")
	}
	return result, nil
}
