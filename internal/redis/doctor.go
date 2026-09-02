// Package redis implements Redis-specific health, capture, and restore operations.
package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

const cliPath = "/usr/bin/redis-cli"

var versionPattern = regexp.MustCompile(`\b([0-9]+\.[0-9]+\.[0-9]+)\b`)

type State struct {
	ServerVersion        string
	Role                 string
	MasterHost           string
	MasterPort           int
	MasterLinkStatus     string
	Lag                  time.Duration
	ReplicationID        string
	ReplicationOffset    int64
	ReadOnly             bool
	Priority             int
	SyncInProgress       bool
	LastSave             int64
	BGSAVEInProgress     bool
	LastBGSAVEStatus     string
	ChangesSinceLastSave int64
	RDBFile              string
	AOFEnabled           bool
}

type Client interface {
	Inspect(context.Context, config.Target, Credentials) (*State, error)
	BGSAVE(context.Context, config.Target, Credentials) error
}

type VersionRunner interface {
	Version(context.Context) (string, error)
}

type CommandVersionRunner struct{}

func (CommandVersionRunner) Version(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, cliPath, "--version").Output()
	if err != nil {
		return "", errors.New("inspect redis-cli version")
	}
	version := versionPattern.FindStringSubmatch(string(output))
	if len(version) != 2 {
		return "", errors.New("redis-cli version is invalid")
	}
	return version[1], nil
}

type CommandClient struct{}

func (CommandClient) command(ctx context.Context, target config.Target, credentials Credentials, arguments ...string) ([]byte, error) {
	args := clientArguments(target, arguments...)
	command := exec.CommandContext(ctx, cliPath, args...)
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "REDISCLI_AUTH=") {
			environment = append(environment, entry)
		}
	}
	command.Env = append(environment, "REDISCLI_AUTH="+credentials.Password)
	output, err := command.Output()
	if err != nil {
		return nil, errors.New("redis-cli command failed")
	}
	return output, nil
}

func clientArguments(target config.Target, arguments ...string) []string {
	args := []string{
		"--no-auth-warning", "--raw",
		"-h", target.Source.Host,
		"-p", strconv.Itoa(target.Source.Port),
		"--user", target.Source.Username,
	}
	return append(args, arguments...)
}

func (client CommandClient) Inspect(ctx context.Context, target config.Target, credentials Credentials) (*State, error) {
	serverOutput, err := client.command(ctx, target, credentials, "INFO", "server")
	if err != nil {
		return nil, err
	}
	replicationOutput, err := client.command(ctx, target, credentials, "INFO", "replication")
	if err != nil {
		return nil, err
	}
	persistenceOutput, err := client.command(ctx, target, credentials, "INFO", "persistence")
	if err != nil {
		return nil, err
	}
	configOutput, err := client.command(ctx, target, credentials, "CONFIG", "GET", "dir", "dbfilename")
	if err != nil {
		return nil, err
	}
	lastSaveOutput, err := client.command(ctx, target, credentials, "LASTSAVE")
	if err != nil {
		return nil, err
	}
	server, err := parseInfo(serverOutput)
	if err != nil {
		return nil, err
	}
	replication, err := parseInfo(replicationOutput)
	if err != nil {
		return nil, err
	}
	persistence, err := parseInfo(persistenceOutput)
	if err != nil {
		return nil, err
	}
	configuration, err := parsePairs(configOutput)
	if err != nil {
		return nil, err
	}
	lastSave, err := strconv.ParseInt(strings.TrimSpace(string(lastSaveOutput)), 10, 64)
	if err != nil || lastSave <= 0 {
		return nil, errors.New("Redis LASTSAVE is invalid")
	}
	dir := configuration["dir"]
	filename := configuration["dbfilename"]
	if !filepath.IsAbs(dir) || filename == "" || filepath.Base(filename) != filename {
		return nil, errors.New("Redis RDB configuration is unsafe")
	}
	state := &State{
		ServerVersion:    server["redis_version"],
		Role:             replication["role"],
		MasterHost:       replication["master_host"],
		MasterLinkStatus: replication["master_link_status"],
		ReplicationID:    replication["master_replid"],
		LastBGSAVEStatus: persistence["rdb_last_bgsave_status"],
		LastSave:         lastSave,
		RDBFile:          filepath.Join(dir, filename),
	}
	if state.MasterPort, err = parseInt(replication, "master_port"); err != nil {
		return nil, err
	}
	lagSeconds, err := parseInt64(replication, "master_last_io_seconds_ago")
	if err != nil || lagSeconds < 0 {
		return nil, errors.New("Redis replication lag is invalid")
	}
	state.Lag = time.Duration(lagSeconds) * time.Second
	if state.ReplicationOffset, err = parseInt64(replication, "slave_repl_offset"); err != nil {
		return nil, err
	}
	readOnly, err := parseInt(replication, "slave_read_only")
	if err != nil {
		return nil, err
	}
	state.ReadOnly = readOnly == 1
	if state.Priority, err = parseInt(replication, "slave_priority"); err != nil {
		return nil, err
	}
	syncing, err := parseInt(replication, "master_sync_in_progress")
	if err != nil {
		return nil, err
	}
	state.SyncInProgress = syncing != 0
	bgsave, err := parseInt(persistence, "rdb_bgsave_in_progress")
	if err != nil {
		return nil, err
	}
	state.BGSAVEInProgress = bgsave != 0
	if state.ChangesSinceLastSave, err = parseInt64(persistence, "rdb_changes_since_last_save"); err != nil {
		return nil, err
	}
	aof, err := parseInt(persistence, "aof_enabled")
	if err != nil {
		return nil, err
	}
	state.AOFEnabled = aof != 0
	return state, nil
}

func (client CommandClient) BGSAVE(ctx context.Context, target config.Target, credentials Credentials) error {
	output, err := client.command(ctx, target, credentials, "BGSAVE", "SCHEDULE")
	if err != nil {
		return err
	}
	reply := strings.TrimSpace(string(output))
	if reply != "Background saving started" && reply != "Background saving scheduled" {
		return errors.New("Redis BGSAVE returned an unexpected response")
	}
	return nil
}

func parseInfo(data []byte) (map[string]string, error) {
	result := make(map[string]string)
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || value == "" {
			return nil, errors.New("Redis INFO contains an invalid field")
		}
		if _, exists := result[key]; exists {
			return nil, errors.New("Redis INFO contains a duplicate field")
		}
		result[key] = value
	}
	return result, nil
}

func parsePairs(data []byte) (map[string]string, error) {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
	if len(lines) == 0 || len(lines)%2 != 0 {
		return nil, errors.New("Redis CONFIG GET returned invalid pairs")
	}
	result := make(map[string]string, len(lines)/2)
	for index := 0; index < len(lines); index += 2 {
		if lines[index] == "" || lines[index+1] == "" {
			return nil, errors.New("Redis CONFIG GET returned an empty field")
		}
		if _, exists := result[lines[index]]; exists {
			return nil, errors.New("Redis CONFIG GET returned a duplicate field")
		}
		result[lines[index]] = lines[index+1]
	}
	return result, nil
}

func parseInt(values map[string]string, key string) (int, error) {
	value, err := parseInt64(values, key)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

func parseInt64(values map[string]string, key string) (int64, error) {
	value, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("Redis status lacks %s", key)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Redis status field %s is invalid", key)
	}
	return parsed, nil
}

type Doctor struct {
	client  Client
	version VersionRunner
}

type Report struct {
	State
	CLIversion string
}

func NewDoctor() *Doctor {
	return &Doctor{client: CommandClient{}, version: CommandVersionRunner{}}
}

func NewDoctorWithDependencies(client Client, version VersionRunner) *Doctor {
	return &Doctor{client: client, version: version}
}

func (doctor *Doctor) Check(ctx context.Context, target config.Target) (*Report, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if doctor == nil || doctor.client == nil || doctor.version == nil {
		return nil, errors.New("Redis doctor dependencies are required")
	}
	credentials, err := ReadCredentialsFile(target.Credentials.File)
	if err != nil {
		return nil, err
	}
	state, err := doctor.client.Inspect(ctx, target, credentials)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("inspect Redis replica: %w", ctx.Err())
		}
		return nil, errors.New("inspect Redis replica")
	}
	if err := evaluateState(target, state); err != nil {
		return nil, err
	}
	version, err := doctor.version.Version(ctx)
	if err != nil {
		return nil, err
	}
	if majorMinor(version) == "" || majorMinor(version) != majorMinor(state.ServerVersion) {
		return nil, errors.New("redis-cli major/minor version does not match Redis server")
	}
	return &Report{State: *state, CLIversion: version}, nil
}

func evaluateState(target config.Target, state *State) error {
	if state == nil || state.ServerVersion == "" {
		return errors.New("Redis server status is incomplete")
	}
	if state.Role != "slave" {
		return errors.New("Redis target is not a replica")
	}
	if state.MasterHost != target.Source.Replica.SourceHost || state.MasterPort != target.Source.Replica.SourcePort {
		return errors.New("Redis replication source does not match configuration")
	}
	if state.MasterLinkStatus != "up" || state.SyncInProgress {
		return errors.New("Redis replication link is not ready")
	}
	maxLag, _ := time.ParseDuration(target.Source.Replica.MaxLag)
	if state.Lag > maxLag {
		return fmt.Errorf("Redis replication lag %s exceeds configured maximum %s", state.Lag, maxLag)
	}
	if !state.ReadOnly {
		return errors.New("Redis backup replica is not read-only")
	}
	if state.Priority != 0 {
		return errors.New("Redis backup replica priority must be zero")
	}
	if state.BGSAVEInProgress || state.LastBGSAVEStatus != "ok" || state.LastSave <= 0 {
		return errors.New("Redis RDB persistence is not ready")
	}
	if filepath.Clean(state.RDBFile) != target.Source.RDBFile {
		return errors.New("Redis configured RDB path does not match source.rdb_file")
	}
	if state.ReplicationID == "" || state.ReplicationOffset < 0 {
		return errors.New("Redis replication coordinates are invalid")
	}
	return nil
}

func majorMinor(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}
