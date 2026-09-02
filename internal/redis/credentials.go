package redis

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

const credentialsFileLimit = 64 << 10

type Credentials struct {
	Password string `yaml:"password"`
}

func ReadCredentials(reader io.Reader) (Credentials, error) {
	data, err := io.ReadAll(io.LimitReader(reader, credentialsFileLimit+1))
	if err != nil {
		return Credentials{}, errors.New("read Redis credentials")
	}
	if len(data) > credentialsFileLimit {
		return Credentials{}, errors.New("Redis credentials file is too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var result Credentials
	if err := decoder.Decode(&result); err != nil {
		return Credentials{}, errors.New("decode Redis credentials")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Credentials{}, errors.New("Redis credentials must contain exactly one YAML document")
	}
	if result.Password == "" || strings.ContainsAny(result.Password, "\r\n") {
		return Credentials{}, errors.New("Redis password must be a non-empty single-line value")
	}
	return result, nil
}

func ReadCredentialsFile(path string) (Credentials, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("inspect Redis credentials: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Credentials{}, errors.New("Redis credentials path must be a regular file, not a symlink")
	}
	if info.Mode().Perm() != 0o600 {
		return Credentials{}, errors.New("Redis credentials file mode must be 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return Credentials{}, errors.New("open Redis credentials")
	}
	defer file.Close()
	return ReadCredentials(file)
}
