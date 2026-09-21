// Package config defines and validates the versioned SaveToA configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	objectpath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	Version     = 1
	MaxFileSize = 1 << 20
)

var (
	namePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	memorySizePattern = regexp.MustCompile(`^[1-9][0-9]*(K|M|G|KiB|MiB|GiB)?$`)
	bucketPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

type Config struct {
	Version     int               `yaml:"config_version"`
	Environment string            `yaml:"environment,omitempty"`
	Targets     map[string]Target `yaml:"targets"`
	Groups      map[string]Group  `yaml:"groups"`
}

type Target struct {
	Driver       string                 `yaml:"driver"`
	Credentials  FileReference          `yaml:"credentials"`
	Source       MariaDBSource          `yaml:"source"`
	Capture      MariaDBCapture         `yaml:"capture"`
	Compression  *Compression           `yaml:"compression,omitempty"`
	Encryption   *Encryption            `yaml:"encryption,omitempty"`
	Destinations map[string]Destination `yaml:"destinations"`
	Retention    *Retention             `yaml:"retention,omitempty"`
}

type FileReference struct {
	File string `yaml:"file"`
}

type MariaDBSource struct {
	Paths                  []string           `yaml:"paths,omitempty"`
	Path                   string             `yaml:"path,omitempty"`
	Socket                 string             `yaml:"socket,omitempty"`
	Host                   string             `yaml:"host,omitempty"`
	Port                   int                `yaml:"port,omitempty"`
	Username               string             `yaml:"username,omitempty"`
	Database               string             `yaml:"database,omitempty"`
	RequireStandby         *bool              `yaml:"require_standby,omitempty"`
	AuthenticationDatabase string             `yaml:"authentication_database,omitempty"`
	RDBFile                string             `yaml:"rdb_file,omitempty"`
	Replica                MariaDBReplicaGate `yaml:"replica"`
}

type MariaDBReplicaGate struct {
	Required            bool   `yaml:"required"`
	SourceHost          string `yaml:"source_host"`
	SourcePort          int    `yaml:"source_port"`
	SourceUser          string `yaml:"source_user"`
	RequireGTID         bool   `yaml:"require_gtid"`
	MaxLag              string `yaml:"max_lag"`
	SetName             string `yaml:"set_name,omitempty"`
	RequireSecondary    bool   `yaml:"require_secondary,omitempty"`
	RequireHidden       bool   `yaml:"require_hidden,omitempty"`
	RequireNonVoting    bool   `yaml:"require_non_voting,omitempty"`
	RequirePriorityZero bool   `yaml:"require_priority_zero,omitempty"`
	RequireReadOnly     bool   `yaml:"require_read_only,omitempty"`
}

type MariaDBCapture struct {
	Prepare           bool   `yaml:"prepare"`
	SafeReplicaBackup bool   `yaml:"safe_replica_backup"`
	UseMemory         string `yaml:"use_memory"`
	Full              bool   `yaml:"full,omitempty"`
	Oplog             bool   `yaml:"oplog,omitempty"`
	BGSAVE            bool   `yaml:"bgsave,omitempty"`
	Schedule          bool   `yaml:"schedule,omitempty"`
	MaxWait           string `yaml:"max_wait,omitempty"`
}

type Compression struct {
	Driver string `yaml:"driver"`
	Level  int    `yaml:"level"`
}

type Encryption struct {
	Driver         string `yaml:"driver"`
	RecipientsFile string `yaml:"recipients_file"`
}

type Destination struct {
	Driver                 string         `yaml:"driver"`
	Path                   string         `yaml:"path,omitempty"`
	Credentials            *FileReference `yaml:"credentials,omitempty"`
	MaintenanceCredentials *FileReference `yaml:"maintenance_credentials,omitempty"`
	Endpoint               string         `yaml:"endpoint,omitempty"`
	Region                 string         `yaml:"region,omitempty"`
	Bucket                 string         `yaml:"bucket,omitempty"`
	Prefix                 string         `yaml:"prefix,omitempty"`
}

type Retention struct {
	KeepDaily   int `yaml:"keep_daily"`
	KeepWeekly  int `yaml:"keep_weekly"`
	KeepMonthly int `yaml:"keep_monthly"`
}

type Group struct {
	Targets []string `yaml:"targets"`
}

func Read(reader io.Reader) (*Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("configuration exceeds %d bytes", MaxFileSize)
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var result Config
	if err := decoder.Decode(&result); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration is empty")
		}
		return nil, fmt.Errorf("decode configuration: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("configuration must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode configuration: %w", err)
	}

	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

func (config Config) Validate() error {
	if config.Version != Version {
		return fmt.Errorf("config_version must be %d", Version)
	}
	if config.Targets == nil {
		return errors.New("targets must be a mapping")
	}
	if config.Groups == nil {
		return errors.New("groups must be a mapping")
	}
	if len(config.Targets) > 0 {
		if err := validateName("environment", config.Environment); err != nil {
			return err
		}
	} else if config.Environment != "" {
		if err := validateName("environment", config.Environment); err != nil {
			return err
		}
	}

	for name, target := range config.Targets {
		if err := validateName("target", name); err != nil {
			return err
		}
		if err := target.validate(); err != nil {
			return fmt.Errorf("target %q: %w", name, err)
		}
	}

	for name, group := range config.Groups {
		if err := validateName("group", name); err != nil {
			return err
		}
		if err := group.validate(config.Targets); err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
	}

	return nil
}

func (target Target) validate() error {
	if target.Driver != "postgresql-base" && target.Driver != "postgresql-dump" &&
		(target.Source.Database != "" || target.Source.RequireStandby != nil) {
		return errors.New("target contains PostgreSQL-only source fields")
	}
	switch target.Driver {
	case "mariadb":
		if err := target.validateMariaDB(); err != nil {
			return err
		}
	case "mongodb":
		if err := target.validateMongoDB(); err != nil {
			return err
		}
	case "redis":
		if err := target.validateRedis(); err != nil {
			return err
		}
	case "tar":
		if err := target.validateTar(); err != nil {
			return err
		}
	case "sqlite3":
		if err := target.validateSQLite3(); err != nil {
			return err
		}
	case "postgresql-base", "postgresql-dump":
		if err := target.validatePostgreSQL(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported capture driver %q", target.Driver)
	}
	return target.validateCommon()
}

func (target Target) validateMariaDB() error {
	if err := target.Credentials.validate("credentials.file"); err != nil {
		return err
	}
	if len(target.Source.Paths) != 0 || target.Source.Path != "" || target.Source.Host != "" || target.Source.Port != 0 || target.Source.Username != "" || target.Source.AuthenticationDatabase != "" || target.Source.RDBFile != "" ||
		target.Source.Replica.SetName != "" || target.Source.Replica.RequireSecondary || target.Source.Replica.RequireHidden ||
		target.Source.Replica.RequireNonVoting || target.Source.Replica.RequirePriorityZero || target.Source.Replica.RequireReadOnly ||
		target.Capture.Full || target.Capture.Oplog || target.Capture.BGSAVE || target.Capture.Schedule || target.Capture.MaxWait != "" {
		return errors.New("mariadb target contains fields for another driver")
	}
	if err := validateAbsolutePath("source.socket", target.Source.Socket, false); err != nil {
		return err
	}
	if !target.Source.Replica.Required {
		return errors.New("source.replica.required must be true for mariadb")
	}
	if strings.TrimSpace(target.Source.Replica.SourceHost) == "" || strings.ContainsAny(target.Source.Replica.SourceHost, "\r\n\t") {
		return errors.New("source.replica.source_host must be a non-empty single-line host")
	}
	if target.Source.Replica.SourcePort < 1 || target.Source.Replica.SourcePort > 65535 {
		return errors.New("source.replica.source_port must be between 1 and 65535")
	}
	if err := validateName("source.replica.source_user", target.Source.Replica.SourceUser); err != nil {
		return err
	}
	if !target.Source.Replica.RequireGTID {
		return errors.New("source.replica.require_gtid must be true for mariadb")
	}
	maxLag, err := time.ParseDuration(target.Source.Replica.MaxLag)
	if err != nil || maxLag <= 0 {
		return errors.New("source.replica.max_lag must be a positive duration")
	}
	if !target.Capture.Prepare {
		return errors.New("capture.prepare must be true for mariadb")
	}
	if !target.Capture.SafeReplicaBackup {
		return errors.New("capture.safe_replica_backup must be true for mariadb")
	}
	if !memorySizePattern.MatchString(target.Capture.UseMemory) {
		return errors.New("capture.use_memory must be a positive size such as 512M")
	}
	return nil
}

func (target Target) validateMongoDB() error {
	if err := target.Credentials.validate("credentials.file"); err != nil {
		return err
	}
	if len(target.Source.Paths) != 0 || target.Source.Path != "" || target.Source.Socket != "" || target.Source.Replica.SourceHost != "" || target.Source.Replica.SourcePort != 0 ||
		target.Source.Replica.SourceUser != "" || target.Source.Replica.RequireGTID || target.Source.Replica.RequireReadOnly ||
		target.Source.RDBFile != "" || target.Capture.Prepare || target.Capture.SafeReplicaBackup || target.Capture.UseMemory != "" ||
		target.Capture.BGSAVE || target.Capture.Schedule || target.Capture.MaxWait != "" {
		return errors.New("mongodb target contains fields for another driver")
	}
	if strings.TrimSpace(target.Source.Host) == "" || strings.ContainsAny(target.Source.Host, "\r\n\t") {
		return errors.New("source.host must be a non-empty single-line host")
	}
	if target.Source.Port < 1 || target.Source.Port > 65535 {
		return errors.New("source.port must be between 1 and 65535")
	}
	if err := validateName("source.username", target.Source.Username); err != nil {
		return err
	}
	if err := validateName("source.authentication_database", target.Source.AuthenticationDatabase); err != nil {
		return err
	}
	gate := target.Source.Replica
	if !gate.Required || !gate.RequireSecondary || !gate.RequireHidden || !gate.RequireNonVoting || !gate.RequirePriorityZero {
		return errors.New("mongodb replica safety gates must all be true")
	}
	if err := validateName("source.replica.set_name", gate.SetName); err != nil {
		return err
	}
	maxLag, err := time.ParseDuration(gate.MaxLag)
	if err != nil || maxLag <= 0 {
		return errors.New("source.replica.max_lag must be a positive duration")
	}
	if !target.Capture.Full || !target.Capture.Oplog {
		return errors.New("mongodb capture.full and capture.oplog must both be true")
	}
	return nil
}

func (target Target) validateRedis() error {
	if err := target.Credentials.validate("credentials.file"); err != nil {
		return err
	}
	if len(target.Source.Paths) != 0 || target.Source.Path != "" || target.Source.Socket != "" || target.Source.AuthenticationDatabase != "" ||
		target.Source.Replica.SourceUser != "" || target.Source.Replica.RequireGTID ||
		target.Source.Replica.SetName != "" || target.Source.Replica.RequireSecondary ||
		target.Source.Replica.RequireHidden || target.Source.Replica.RequireNonVoting ||
		target.Capture.Prepare || target.Capture.SafeReplicaBackup || target.Capture.UseMemory != "" ||
		target.Capture.Full || target.Capture.Oplog {
		return errors.New("redis target contains fields for another driver")
	}
	if strings.TrimSpace(target.Source.Host) == "" || strings.ContainsAny(target.Source.Host, "\r\n\t") {
		return errors.New("source.host must be a non-empty single-line host")
	}
	if target.Source.Port < 1 || target.Source.Port > 65535 {
		return errors.New("source.port must be between 1 and 65535")
	}
	if err := validateName("source.username", target.Source.Username); err != nil {
		return err
	}
	if err := validateAbsolutePath("source.rdb_file", target.Source.RDBFile, false); err != nil {
		return err
	}
	gate := target.Source.Replica
	if !gate.Required || !gate.RequireReadOnly || !gate.RequirePriorityZero {
		return errors.New("redis replica safety gates must all be true")
	}
	if strings.TrimSpace(gate.SourceHost) == "" || strings.ContainsAny(gate.SourceHost, "\r\n\t") {
		return errors.New("source.replica.source_host must be a non-empty single-line host")
	}
	if gate.SourcePort < 1 || gate.SourcePort > 65535 {
		return errors.New("source.replica.source_port must be between 1 and 65535")
	}
	maxLag, err := time.ParseDuration(gate.MaxLag)
	if err != nil || maxLag <= 0 {
		return errors.New("source.replica.max_lag must be a positive duration")
	}
	maxWait, err := time.ParseDuration(target.Capture.MaxWait)
	if err != nil || maxWait <= 0 || maxWait > 24*time.Hour {
		return errors.New("capture.max_wait must be a positive duration no greater than 24h")
	}
	if !target.Capture.BGSAVE || !target.Capture.Schedule {
		return errors.New("redis capture.bgsave and capture.schedule must both be true")
	}
	return nil
}

func (target Target) validateTar() error {
	if target.Credentials.File != "" {
		return errors.New("tar target must not contain credentials")
	}
	if target.Source.Path != "" || target.Source.Socket != "" || target.Source.Host != "" || target.Source.Port != 0 ||
		target.Source.Username != "" || target.Source.AuthenticationDatabase != "" || target.Source.RDBFile != "" ||
		target.Source.Replica != (MariaDBReplicaGate{}) {
		return errors.New("tar target contains fields for another driver")
	}
	if target.Capture != (MariaDBCapture{}) {
		return errors.New("tar target contains capture options")
	}
	if len(target.Source.Paths) == 0 || len(target.Source.Paths) > 128 {
		return errors.New("source.paths must contain 1 to 128 paths for tar")
	}
	paths := append([]string(nil), target.Source.Paths...)
	for index, sourcePath := range paths {
		if err := validateAbsolutePath(fmt.Sprintf("source.paths[%d]", index), sourcePath, true); err != nil {
			return err
		}
	}
	sort.Strings(paths)
	for index := 1; index < len(paths); index++ {
		if paths[index] == paths[index-1] || strings.HasPrefix(paths[index], paths[index-1]+string(filepath.Separator)) {
			return errors.New("source.paths must not contain duplicate or overlapping paths")
		}
	}
	return nil
}

func (target Target) validateSQLite3() error {
	if target.Credentials.File != "" {
		return errors.New("sqlite3 target must not contain credentials")
	}
	if len(target.Source.Paths) != 0 || target.Source.Socket != "" || target.Source.Host != "" || target.Source.Port != 0 ||
		target.Source.Username != "" || target.Source.AuthenticationDatabase != "" || target.Source.RDBFile != "" ||
		target.Source.Replica != (MariaDBReplicaGate{}) {
		return errors.New("sqlite3 target contains fields for another driver")
	}
	if target.Capture != (MariaDBCapture{}) {
		return errors.New("sqlite3 target contains capture options")
	}
	return validateAbsolutePath("source.path", target.Source.Path, true)
}

func (target Target) validatePostgreSQL() error {
	if err := target.Credentials.validate("credentials.file"); err != nil {
		return err
	}
	if len(target.Source.Paths) != 0 || target.Source.Path != "" || target.Source.Socket != "" ||
		target.Source.AuthenticationDatabase != "" || target.Source.RDBFile != "" ||
		target.Source.Replica != (MariaDBReplicaGate{}) || target.Capture != (MariaDBCapture{}) {
		return errors.New("postgresql target contains fields for another driver")
	}
	if target.Source.Host != "localhost" && target.Source.Host != "127.0.0.1" {
		return errors.New("source.host must be a loopback host for PostgreSQL")
	}
	if target.Source.Port < 1 || target.Source.Port > 65535 {
		return errors.New("source.port must be between 1 and 65535")
	}
	if err := validateName("source.username", target.Source.Username); err != nil {
		return err
	}
	if target.Driver == "postgresql-base" {
		if target.Source.RequireStandby == nil || !*target.Source.RequireStandby || target.Source.Database != "" {
			return errors.New("postgresql-base requires source.require_standby and forbids source.database")
		}
	} else {
		if target.Source.RequireStandby != nil {
			return errors.New("postgresql-dump does not accept source.require_standby")
		}
		if err := validateName("source.database", target.Source.Database); err != nil {
			return err
		}
	}
	return nil
}

func (target Target) validateCommon() error {
	if target.Compression != nil {
		if target.Compression.Driver != "zstd" {
			return fmt.Errorf("unsupported compression driver %q", target.Compression.Driver)
		}
		if target.Compression.Level < -5 || target.Compression.Level > 22 {
			return errors.New("compression.level must be between -5 and 22")
		}
	}
	if target.Encryption != nil {
		if target.Encryption.Driver != "age" {
			return fmt.Errorf("unsupported encryption driver %q", target.Encryption.Driver)
		}
		if err := validateAbsolutePath("encryption.recipients_file", target.Encryption.RecipientsFile, false); err != nil {
			return err
		}
	}
	if len(target.Destinations) == 0 {
		return errors.New("destinations must contain at least one destination")
	}
	for name, destination := range target.Destinations {
		if err := validateName("destination", name); err != nil {
			return err
		}
		if err := destination.validate(); err != nil {
			return fmt.Errorf("destination %q: %w", name, err)
		}
		if target.Retention != nil && destination.Driver == "s3" {
			if destination.MaintenanceCredentials == nil {
				return fmt.Errorf("destination %q: maintenance_credentials.file is required when retention is configured", name)
			}
		}
	}
	if target.Retention != nil {
		if err := target.Retention.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (reference FileReference) validate(field string) error {
	return validateAbsolutePath(field, reference.File, false)
}

func (destination Destination) validate() error {
	switch destination.Driver {
	case "local":
		if err := validateAbsolutePath("path", destination.Path, true); err != nil {
			return err
		}
		if destination.Credentials != nil || destination.MaintenanceCredentials != nil || destination.Endpoint != "" || destination.Region != "" || destination.Bucket != "" || destination.Prefix != "" {
			return errors.New("local destination contains S3-only fields")
		}
	case "s3":
		if destination.Path != "" {
			return errors.New("s3 destination contains local-only field path")
		}
		if destination.Credentials == nil {
			return errors.New("credentials.file is required for s3")
		}
		if err := destination.Credentials.validate("credentials.file"); err != nil {
			return err
		}
		if destination.MaintenanceCredentials != nil {
			if err := destination.MaintenanceCredentials.validate("maintenance_credentials.file"); err != nil {
				return err
			}
			if destination.MaintenanceCredentials.File == destination.Credentials.File {
				return errors.New("maintenance credentials must be separate from upload credentials")
			}
		}
		endpoint, err := url.Parse(destination.Endpoint)
		if err != nil || !validS3Endpoint(endpoint) {
			return errors.New("endpoint must be an HTTPS origin, or an HTTP origin on a loopback host, without credentials, path, query, or fragment")
		}
		if !regionPattern.MatchString(destination.Region) {
			return errors.New("region must be a lowercase S3 signing region")
		}
		if !bucketPattern.MatchString(destination.Bucket) || strings.Contains(destination.Bucket, "..") {
			return errors.New("bucket must be a lowercase DNS-compatible name of 3 to 63 characters")
		}
		if destination.Prefix != "" && (strings.HasPrefix(destination.Prefix, "/") ||
			strings.Contains(destination.Prefix, `\`) || objectpath.Clean(destination.Prefix) != destination.Prefix) {
			return errors.New("prefix must be a clean relative object-key prefix")
		}
	default:
		return fmt.Errorf("unsupported destination driver %q", destination.Driver)
	}
	return nil
}

func validS3Endpoint(endpoint *url.URL) bool {
	if endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return false
	}
	if endpoint.Scheme == "https" {
		return true
	}
	if endpoint.Scheme != "http" {
		return false
	}
	host := endpoint.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func (retention Retention) validate() error {
	if retention.KeepDaily < 0 || retention.KeepWeekly < 0 || retention.KeepMonthly < 0 {
		return errors.New("retention counts must not be negative")
	}
	if retention.KeepDaily == 0 && retention.KeepWeekly == 0 && retention.KeepMonthly == 0 {
		return errors.New("retention must keep at least one completed backup")
	}
	return nil
}

func (group Group) validate(targets map[string]Target) error {
	if len(group.Targets) == 0 {
		return errors.New("targets must contain at least one target")
	}
	seen := make(map[string]struct{}, len(group.Targets))
	for _, target := range group.Targets {
		if _, ok := targets[target]; !ok {
			return fmt.Errorf("references unknown target %q", target)
		}
		if _, ok := seen[target]; ok {
			return fmt.Errorf("contains duplicate target %q", target)
		}
		seen[target] = struct{}{}
	}
	return nil
}

func validateName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%s name %q must match %s", kind, name, namePattern.String())
	}
	return nil
}

func validateAbsolutePath(field, path string, rejectRoot bool) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%s must be a clean absolute path", field)
	}
	if rejectRoot && path == string(filepath.Separator) {
		return fmt.Errorf("%s must not be the filesystem root", field)
	}
	return nil
}
