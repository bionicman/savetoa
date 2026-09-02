// Package mariadb implements MariaDB-specific health and capture operations.
package mariadb

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

const (
	clientPath = "/usr/bin/mariadb"
	backupPath = "/usr/bin/mariadb-backup"
)

var versionPattern = regexp.MustCompile(`\b([0-9]+\.[0-9]+\.[0-9]+)\b`)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		// Native stderr is deliberately discarded: option files and client errors
		// can contain secret-bearing material. Arguments are never included.
		return nil, fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return output, nil
}

type Doctor struct {
	runner Runner
}

type Report struct {
	ServerVersion string
	BackupVersion string
	SourceHost    string
	SourcePort    int
	SourceUser    string
	GTIDPosition  string
	Lag           time.Duration
}

func NewDoctor() *Doctor {
	return &Doctor{runner: CommandRunner{}}
}

func NewDoctorWithRunner(runner Runner) *Doctor {
	return &Doctor{runner: runner}
}

func (doctor *Doctor) Check(ctx context.Context, target config.Target) (*Report, error) {
	if doctor == nil || doctor.runner == nil {
		return nil, errors.New("MariaDB doctor has no command runner")
	}
	if err := validateCredentialFile(target.Credentials.File); err != nil {
		return nil, err
	}

	clientArgs := []string{
		"--defaults-extra-file=" + target.Credentials.File,
		"--protocol=SOCKET",
		"--socket=" + target.Source.Socket,
		"--batch",
		"--skip-column-names",
		"--raw",
	}
	serverOutput, err := doctor.runner.Run(ctx, clientPath, append(clientArgs,
		"--execute=SELECT @@version, @@global.read_only, @@global.gtid_strict_mode, @@global.server_id, @@socket")...)
	if err != nil {
		return nil, commandFailure(ctx, "inspect MariaDB server")
	}
	serverVersion, err := validateServer(string(serverOutput), target.Source.Socket)
	if err != nil {
		return nil, err
	}

	replicaArgs := []string{
		"--defaults-extra-file=" + target.Credentials.File,
		"--protocol=SOCKET",
		"--socket=" + target.Source.Socket,
		"--batch",
		"--raw",
		"--vertical",
		"--execute=SHOW REPLICA STATUS",
	}
	replicaOutput, err := doctor.runner.Run(ctx, clientPath, replicaArgs...)
	if err != nil {
		return nil, commandFailure(ctx, "inspect MariaDB replica")
	}
	replica, err := validateReplica(string(replicaOutput), target.Source.Replica)
	if err != nil {
		return nil, err
	}

	backupOutput, err := doctor.runner.Run(ctx, backupPath, "--version")
	if err != nil {
		return nil, commandFailure(ctx, "inspect MariaDB Backup")
	}
	backupVersion, err := extractVersion(string(backupOutput))
	if err != nil {
		return nil, fmt.Errorf("inspect MariaDB Backup: %w", err)
	}
	if serverVersion != backupVersion {
		return nil, fmt.Errorf("MariaDB server version %s does not match mariadb-backup version %s", serverVersion, backupVersion)
	}

	return &Report{
		ServerVersion: serverVersion,
		BackupVersion: backupVersion,
		SourceHost:    replica["Master_Host"],
		SourcePort:    target.Source.Replica.SourcePort,
		SourceUser:    replica["Master_User"],
		GTIDPosition:  replica["Gtid_IO_Pos"],
		Lag:           time.Duration(mustInt(replica["Seconds_Behind_Master"])) * time.Second,
	}, nil
}

func validateCredentialFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect MariaDB credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("MariaDB credential file must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("MariaDB credential file must not be accessible by group or others")
	}
	if info.Size() == 0 {
		return errors.New("MariaDB credential file is empty")
	}
	return nil
}

func validateServer(output, expectedSocket string) (string, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		return "", errors.New("MariaDB server query returned an unexpected row count")
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 5 {
		return "", errors.New("MariaDB server query returned an unexpected field count")
	}
	if !isTrue(fields[1]) {
		return "", errors.New("MariaDB source is not read-only")
	}
	if !isTrue(fields[2]) {
		return "", errors.New("MariaDB GTID strict mode is disabled")
	}
	serverID, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil || serverID == 0 {
		return "", errors.New("MariaDB server_id is missing or invalid")
	}
	if fields[4] != expectedSocket {
		return "", errors.New("MariaDB server socket does not match target configuration")
	}
	return extractVersion(fields[0])
}

func validateReplica(output string, gate config.MariaDBReplicaGate) (map[string]string, error) {
	rows := 0
	values := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "***") && strings.Contains(line, "row") {
			rows++
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			values[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	if rows != 1 {
		return nil, errors.New("MariaDB must have exactly one configured replica channel")
	}
	if values["Master_Host"] != gate.SourceHost || values["Master_User"] != gate.SourceUser {
		return nil, errors.New("MariaDB replica source identity does not match target configuration")
	}
	port, err := strconv.Atoi(values["Master_Port"])
	if err != nil || port != gate.SourcePort {
		return nil, errors.New("MariaDB replica source port does not match target configuration")
	}
	if values["Slave_IO_Running"] != "Yes" || values["Slave_SQL_Running"] != "Yes" {
		return nil, errors.New("MariaDB replica threads are not both running")
	}
	if values["Last_IO_Errno"] != "0" || values["Last_SQL_Errno"] != "0" {
		return nil, errors.New("MariaDB replica reports an IO or SQL error")
	}
	usingGTID := strings.ToLower(values["Using_Gtid"])
	if gate.RequireGTID && usingGTID != "slave_pos" && usingGTID != "replica_pos" {
		return nil, errors.New("MariaDB replica is not using replica GTID position")
	}
	if gate.RequireGTID && values["Gtid_IO_Pos"] == "" {
		return nil, errors.New("MariaDB replica GTID position is empty")
	}
	lagSeconds, err := strconv.Atoi(values["Seconds_Behind_Master"])
	if err != nil || lagSeconds < 0 {
		return nil, errors.New("MariaDB replica lag is unavailable")
	}
	maxLag, _ := time.ParseDuration(gate.MaxLag)
	if time.Duration(lagSeconds)*time.Second > maxLag {
		return nil, fmt.Errorf("MariaDB replica lag exceeds configured maximum %s", gate.MaxLag)
	}
	return values, nil
}

func extractVersion(value string) (string, error) {
	match := versionPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return "", errors.New("version output is not recognized")
	}
	return match[1], nil
}

func isTrue(value string) bool {
	return value == "1" || strings.EqualFold(value, "ON") || strings.EqualFold(value, "YES")
}

func mustInt(value string) int {
	result, _ := strconv.Atoi(value)
	return result
}

func commandFailure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return errors.New(operation + " failed")
}
