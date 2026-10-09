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
        maintenance_credentials:
          file: /etc/savetoa/maintenance-credentials.d/offsite-s3.yml
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

const validMongoConfig = `
config_version: 1
environment: example
targets:
  example-mongodb:
    driver: mongodb
    credentials:
      file: /etc/savetoa/credentials.d/example-mongodb.yml
    source:
      host: 127.0.0.1
      port: 27017
      username: savetoa_backup
      authentication_database: admin
      replica:
        required: true
        set_name: example-production
        require_secondary: true
        require_hidden: true
        require_non_voting: true
        require_priority_zero: true
        max_lag: 5m
    capture:
      full: true
      oplog: true
    compression:
      driver: zstd
      level: 3
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
groups: {}
`

const validMariaDBDumpConfig = `
config_version: 1
environment: example
targets:
  mailserver:
    driver: mariadb-dump
    credentials:
      file: /etc/savetoa/credentials.d/mailserver.cnf
    source:
      socket: /run/mysqld/mysqld.sock
      database: mailserver
    compression:
      driver: zstd
      level: 3
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
groups: {}
`

const validRedisConfig = `
config_version: 1
environment: example
targets:
  example-redis:
    driver: redis
    credentials:
      file: /etc/savetoa/credentials.d/example-redis.yml
    source:
      host: 127.0.0.1
      port: 6379
      username: savetoa_backup
      rdb_file: /var/lib/redis/dump.rdb
      replica:
        required: true
        source_host: redis-primary.internal
        source_port: 6379
        require_read_only: true
        require_priority_zero: true
        max_lag: 5s
    capture:
      bgsave: true
      schedule: true
      max_wait: 10m
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
groups: {}
`

const validTarConfig = `
config_version: 1
environment: example
targets:
  example-acme:
    driver: tar
    source:
      paths:
        - /etc/letsencrypt
        - /var/lib/acme.sh
    compression:
      driver: zstd
      level: 3
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
groups: {}
`

const validSQLite3Config = `
config_version: 1
environment: example
targets:
  example-sqlite:
    driver: sqlite3
    source:
      path: /var/lib/example/database.sqlite3
    compression:
      driver: zstd
      level: 3
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
groups: {}
`

const validPostgreSQLConfig = `
config_version: 1
environment: example
targets:
  cluster:
    driver: postgresql-base
    credentials:
      file: /etc/savetoa/credentials.d/cluster.pgpass
    source:
      host: 127.0.0.1
      port: 5432
      username: backup
      require_standby: true
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
  database:
    driver: postgresql-dump
    credentials:
      file: /etc/savetoa/credentials.d/database.pgpass
    source:
      host: 127.0.0.1
      port: 5432
      username: backup
      database: appdb
    destinations:
      local:
        driver: local
        path: /var/backups/savetoa
groups: {}
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

func TestParseValidMongoDBConfiguration(t *testing.T) {
	parsed, err := Parse([]byte(validMongoConfig))
	if err != nil {
		t.Fatalf("Parse(valid MongoDB) error = %v", err)
	}
	target := parsed.Targets["example-mongodb"]
	if target.Driver != "mongodb" || target.Source.Replica.SetName != "example-production" || !target.Capture.Oplog {
		t.Fatalf("MongoDB target = %#v", target)
	}
}

func TestParseValidMariaDBDumpConfiguration(t *testing.T) {
	parsed, err := Parse([]byte(validMariaDBDumpConfig))
	if err != nil {
		t.Fatalf("Parse(valid MariaDB dump) error = %v", err)
	}
	target := parsed.Targets["mailserver"]
	if target.Driver != "mariadb-dump" || target.Source.Database != "mailserver" || target.Source.Socket != "/run/mysqld/mysqld.sock" {
		t.Fatalf("MariaDB dump target = %#v", target)
	}
}

func TestMariaDBDumpConfigurationFailsClosed(t *testing.T) {
	tests := map[string]string{
		"missing database":   strings.Replace(validMariaDBDumpConfig, "      database: mailserver\n", "", 1),
		"multiple databases": strings.Replace(validMariaDBDumpConfig, "      database: mailserver", "      database: mailserver\n      databases: [roundcube]", 1),
		"remote host":        strings.Replace(validMariaDBDumpConfig, "      socket: /run/mysqld/mysqld.sock", "      socket: /run/mysqld/mysqld.sock\n      host: database.example", 1),
		"capture options":    strings.Replace(validMariaDBDumpConfig, "    compression:", "    capture:\n      prepare: true\n    compression:", 1),
		"unsafe database":    strings.Replace(validMariaDBDumpConfig, "database: mailserver", "database: ../mailserver", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse(unsafe MariaDB dump) succeeded")
			}
		})
	}
}

func TestMongoDBConfigurationFailsClosed(t *testing.T) {
	tests := map[string]string{
		"not secondary": strings.Replace(validMongoConfig, "require_secondary: true", "require_secondary: false", 1),
		"voting":        strings.Replace(validMongoConfig, "require_non_voting: true", "require_non_voting: false", 1),
		"filtered":      strings.Replace(validMongoConfig, "full: true", "full: false", 1),
		"no oplog":      strings.Replace(validMongoConfig, "oplog: true", "oplog: false", 1),
		"cross driver":  strings.Replace(validMongoConfig, "full: true", "full: true\n      prepare: true", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse(unsafe MongoDB) succeeded")
			}
		})
	}
}

func TestParseValidRedisConfiguration(t *testing.T) {
	parsed, err := Parse([]byte(validRedisConfig))
	if err != nil {
		t.Fatalf("Parse(valid Redis) error = %v", err)
	}
	target := parsed.Targets["example-redis"]
	if target.Driver != "redis" || target.Source.RDBFile != "/var/lib/redis/dump.rdb" || !target.Capture.Schedule {
		t.Fatalf("Redis target = %#v", target)
	}
}

func TestRedisConfigurationFailsClosed(t *testing.T) {
	tests := map[string]string{
		"writable replica": strings.Replace(validRedisConfig, "require_read_only: true", "require_read_only: false", 1),
		"promotable":       strings.Replace(validRedisConfig, "require_priority_zero: true", "require_priority_zero: false", 1),
		"no scheduling":    strings.Replace(validRedisConfig, "schedule: true", "schedule: false", 1),
		"unbounded wait":   strings.Replace(validRedisConfig, "max_wait: 10m", "max_wait: 0s", 1),
		"relative rdb":     strings.Replace(validRedisConfig, "/var/lib/redis/dump.rdb", "dump.rdb", 1),
		"cross driver":     strings.Replace(validRedisConfig, "bgsave: true", "bgsave: true\n      oplog: true", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse(unsafe Redis) succeeded")
			}
		})
	}
}

func TestParseValidTarConfiguration(t *testing.T) {
	parsed, err := Parse([]byte(validTarConfig))
	if err != nil {
		t.Fatalf("Parse(valid tar) error = %v", err)
	}
	target := parsed.Targets["example-acme"]
	if target.Driver != "tar" || len(target.Source.Paths) != 2 {
		t.Fatalf("tar target = %#v", target)
	}
}

func TestTarConfigurationFailsClosed(t *testing.T) {
	tests := map[string]string{
		"missing paths": strings.Replace(validTarConfig, "      paths:\n        - /etc/letsencrypt\n        - /var/lib/acme.sh\n", "", 1),
		"root path":     strings.Replace(validTarConfig, "/etc/letsencrypt", "/", 1),
		"relative path": strings.Replace(validTarConfig, "/etc/letsencrypt", "etc/letsencrypt", 1),
		"overlap":       strings.Replace(validTarConfig, "/var/lib/acme.sh", "/etc/letsencrypt/live", 1),
		"credentials":   strings.Replace(validTarConfig, "    source:\n", "    credentials:\n      file: /etc/savetoa/credentials.d/forbidden\n    source:\n", 1),
		"capture args":  strings.Replace(validTarConfig, "    compression:\n", "    capture:\n      full: true\n    compression:\n", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse(unsafe tar) succeeded")
			}
		})
	}
}

func TestParseValidSQLite3Configuration(t *testing.T) {
	parsed, err := Parse([]byte(validSQLite3Config))
	if err != nil {
		t.Fatalf("Parse(valid SQLite3) error = %v", err)
	}
	target := parsed.Targets["example-sqlite"]
	if target.Driver != "sqlite3" || target.Source.Path != "/var/lib/example/database.sqlite3" {
		t.Fatalf("SQLite3 target = %#v", target)
	}
}

func TestSQLite3ConfigurationFailsClosed(t *testing.T) {
	tests := map[string]string{
		"missing path":  strings.Replace(validSQLite3Config, "    source:\n      path: /var/lib/example/database.sqlite3\n", "", 1),
		"root path":     strings.Replace(validSQLite3Config, "/var/lib/example/database.sqlite3", "/", 1),
		"relative path": strings.Replace(validSQLite3Config, "/var/lib/example/database.sqlite3", "database.sqlite3", 1),
		"credentials":   strings.Replace(validSQLite3Config, "    source:\n", "    credentials:\n      file: /etc/savetoa/credentials.d/forbidden\n    source:\n", 1),
		"paths":         strings.Replace(validSQLite3Config, "      path: /var/lib/example/database.sqlite3", "      path: /var/lib/example/database.sqlite3\n      paths: [/var/lib/example]", 1),
		"capture args":  strings.Replace(validSQLite3Config, "    compression:\n", "    capture:\n      full: true\n    compression:\n", 1),
		"unknown field": strings.Replace(validSQLite3Config, "      path: /var/lib/example/database.sqlite3", "      path: /var/lib/example/database.sqlite3\n      immutable: true", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse(unsafe SQLite3) succeeded")
			}
		})
	}
}

func TestPostgreSQLConfigurationModesAndFailClosed(t *testing.T) {
	parsed, err := Parse([]byte(validPostgreSQLConfig))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Targets["cluster"].Source.RequireStandby == nil || !*parsed.Targets["cluster"].Source.RequireStandby || parsed.Targets["database"].Source.Database != "appdb" {
		t.Fatal("PostgreSQL modes not decoded")
	}
	tests := map[string]string{
		"base without standby":           strings.Replace(validPostgreSQLConfig, "      require_standby: true\n", "", 1),
		"dump with standby":              strings.Replace(validPostgreSQLConfig, "      database: appdb\n", "      database: appdb\n      require_standby: true\n", 1),
		"dump with false standby option": strings.Replace(validPostgreSQLConfig, "      database: appdb\n", "      database: appdb\n      require_standby: false\n", 1),
		"dump without database":          strings.Replace(validPostgreSQLConfig, "      database: appdb\n", "", 1),
		"connection string host":         strings.Replace(validPostgreSQLConfig, "host: 127.0.0.1", "host: 'postgresql://evil'", 1),
		"remote host":                    strings.Replace(validPostgreSQLConfig, "host: 127.0.0.1", "host: db.internal", 1),
		"foreign capture option":         strings.Replace(validPostgreSQLConfig, "    destinations:\n", "    capture:\n      full: true\n    destinations:\n", 1),
		"unknown field":                  strings.Replace(validPostgreSQLConfig, "      database: appdb\n", "      database: appdb\n      command: pg_dump --password=secret\n", 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("unsafe PostgreSQL configuration accepted")
			}
		})
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
		"insecure remote endpoint": {
			data: strings.Replace(validConfig, "https://s3.example.invalid", "http://s3.example.invalid", 1),
			want: "endpoint must be an HTTPS origin",
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

func TestParseAllowsLoopbackHTTPForS3(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:3900",
		"http://127.0.0.1:3900",
		"http://[::1]:3900",
	} {
		t.Run(endpoint, func(t *testing.T) {
			data := strings.Replace(validConfig, "https://s3.example.invalid", endpoint, 1)
			if _, err := Parse([]byte(data)); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseRejectsNonLoopbackHTTPForS3(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost.example:3900",
		"http://127.0.0.2.example:3900",
		"http://0.0.0.0:3900",
		"http://[::]:3900",
	} {
		t.Run(endpoint, func(t *testing.T) {
			data := strings.Replace(validConfig, "https://s3.example.invalid", endpoint, 1)
			_, err := Parse([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "endpoint must be an HTTPS origin") {
				t.Fatalf("Parse() error = %v, want loopback HTTP rejection", err)
			}
		})
	}
}

func TestRetentionRequiresSeparateS3MaintenanceCredentials(t *testing.T) {
	missing := strings.Replace(validConfig, "        maintenance_credentials:\n          file: /etc/savetoa/maintenance-credentials.d/offsite-s3.yml\n", "", 1)
	if _, err := Parse([]byte(missing)); err == nil || !strings.Contains(err.Error(), "maintenance_credentials.file is required") {
		t.Fatalf("Parse(missing maintenance credentials) error = %v", err)
	}
	shared := strings.Replace(validConfig, "/etc/savetoa/maintenance-credentials.d/offsite-s3.yml", "/etc/savetoa/credentials.d/offsite-s3.yml", 1)
	if _, err := Parse([]byte(shared)); err == nil || !strings.Contains(err.Error(), "must be separate") {
		t.Fatalf("Parse(shared maintenance credentials) error = %v", err)
	}
}

func TestReadRejectsOversizedConfiguration(t *testing.T) {
	data := strings.Repeat("x", MaxFileSize+1)
	_, err := Read(strings.NewReader(data))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Read(oversized) error = %v, want size error", err)
	}
}
