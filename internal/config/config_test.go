package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
config_version: 1
environment: example
targets:
  example-mariadb:
    driver: mariadb
    credentials:
      file: /etc/savetoa/credentials.d/example-mariadb.cnf
    source:
      socket: /run/mysqld/mysqld.sock
      replica:
        required: true
        source_host: db-primary.internal
        source_port: 3306
        source_user: backup_replication
        require_gtid: true
        max_lag: 5m
    capture:
      prepare: true
      safe_replica_backup: true
      use_memory: 512M
    compression:
      driver: zstd
      level: 3
    encryption:
      driver: age
      recipients_file: /etc/savetoa/recipients.d/example.age
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
      offsite:
        driver: s3
        credentials:
          file: /etc/savetoa/credentials.d/offsite-s3.yml
        endpoint: https://s3.example.invalid
        region: eu-west-1
        bucket: example-backups
        prefix: databases
    retention:
      keep_daily: 14
      keep_weekly: 8
      keep_monthly: 12
groups:
  example-databases:
    targets:
      - example-mariadb
`

func TestParseValidConfiguration(t *testing.T) {
	parsed, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse(valid) error = %v", err)
	}
	if parsed.Version != Version {
		t.Fatalf("Version = %d, want %d", parsed.Version, Version)
	}
	if parsed.Targets["example-mariadb"].Driver != "mariadb" {
		t.Fatal("mariadb target was not decoded")
	}
}

func TestPackagedExampleMatchesSchema(t *testing.T) {
	path := filepath.Join("..", "..", "packaging", "config.example.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse(packaged example) error = %v", err)
	}
}

func TestEmptyInstalledConfigurationIsValid(t *testing.T) {
	data := []byte("config_version: 1\ntargets: {}\ngroups: {}\n")
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse(empty installed configuration) error = %v", err)
	}
}

func TestParseFailsClosed(t *testing.T) {
	tests := map[string]struct {
		data string
		want string
	}{
		"unknown top-level field": {
			data: "config_version: 1\ntargets: {}\ngroups: {}\nschedule: daily\n",
			want: "field schedule not found",
		},
		"duplicate field": {
			data: "config_version: 1\nconfig_version: 1\ntargets: {}\ngroups: {}\n",
			want: "mapping key \"config_version\" already defined",
		},
		"inline secret": {
			data: strings.Replace(validConfig, "file: /etc/savetoa/credentials.d/example-mariadb.cnf", "file: /etc/savetoa/credentials.d/example-mariadb.cnf\n      password: forbidden", 1),
			want: "field password not found",
		},
		"unknown driver option": {
			data: strings.Replace(validConfig, "use_memory: 512M", "use_memory: 512M\n      preparee: true", 1),
			want: "field preparee not found",
		},
		"unsafe capture": {
			data: strings.Replace(validConfig, "prepare: true", "prepare: false", 1),
			want: "capture.prepare must be true",
		},
		"missing environment": {
			data: strings.Replace(validConfig, "environment: example\n", "", 1),
			want: "environment name",
		},
		"cross-driver option": {
			data: strings.Replace(validConfig, "path: /var/backups/savetoa", "path: /var/backups/savetoa\n        bucket: misplaced", 1),
			want: "local destination contains S3-only fields",
		},
		"insecure endpoint": {
			data: strings.Replace(validConfig, "https://s3.example.invalid", "http://s3.example.invalid", 1),
			want: "endpoint must be an HTTPS URL",
		},
		"unknown group target": {
			data: strings.Replace(validConfig, "- example-mariadb", "- missing-target", 1),
			want: "references unknown target",
		},
		"multiple documents": {
			data: "config_version: 1\ntargets: {}\ngroups: {}\n---\nconfig_version: 1\ntargets: {}\ngroups: {}\n",
			want: "exactly one YAML document",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(test.data))
			if err == nil {
				t.Fatal("Parse() error = nil")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %q, want substring %q", err, test.want)
			}
		})
	}
}

func TestReadRejectsOversizedConfiguration(t *testing.T) {
	data := strings.Repeat("x", MaxFileSize+1)
	_, err := Read(strings.NewReader(data))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Read(oversized) error = %v, want size error", err)
	}
}
