package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/config"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/mariadb"
	"github.com/bionicman/savetoa/internal/mongodb"
	"github.com/bionicman/savetoa/internal/restore"
)

func TestHelpIsSuccessful(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run(help) code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "run TARGET") {
		t.Fatalf("help does not describe target execution: %q", stdout.String())
	}
}

func TestDoctorRejectsUnknownTargetBeforeExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("config_version: 1\ntargets: {}\ngroups: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run([]string{"--config", path, "doctor", "missing"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "not configured") {
		t.Fatalf("doctor code=%d stderr=%q", code, stderr.String())
	}
}

func TestVersionIsSuccessful(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run(--version) code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout.String(), "savetoa ") {
		t.Fatalf("unexpected version output: %q", stdout.String())
	}
}

func TestPlannedCommandFailsClosed(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"prune", "production-mariadb"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("unimplemented backup command returned success")
	}
	if !strings.Contains(stderr.String(), "not implemented") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run(unknown) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}

func TestBackupSetRelativePathUsesBackupIDDate(t *testing.T) {
	path, err := backupSetRelativePath("production", "production-mariadb", "20260902t181123z-6604825f507250fc")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("production", "production-mariadb", "2026", "09", "02", "20260902t181123z-6604825f507250fc")
	if path != want {
		t.Fatalf("backupSetRelativePath() = %q, want %q", path, want)
	}
}

func TestBackupSetRelativePathRejectsInvalidID(t *testing.T) {
	for _, backupID := range []string{"short", "20260230t000000z-0000000000000000", "20260902/escape"} {
		if _, err := backupSetRelativePath("production", "production-mariadb", backupID); err == nil {
			t.Fatalf("backupSetRelativePath(%q) succeeded", backupID)
		}
	}
}

type pipelineRunner struct {
	calls int
}

func (runner *pipelineRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls++
	if name == "/usr/bin/mariadb" {
		if slices.Contains(args, "--vertical") {
			return []byte(`*************************** 1. row ***************************
Master_Host: fd00::10
Master_User: backup_replication
Master_Port: 3306
Slave_IO_Running: Yes
Slave_SQL_Running: Yes
Seconds_Behind_Master: 0
Last_IO_Errno: 0
Last_SQL_Errno: 0
Using_Gtid: Slave_Pos
Gtid_IO_Pos: 1-2-12
`), nil
		}
		for _, argument := range args {
			if strings.HasPrefix(argument, "--execute=SELECT ") {
				return []byte("12.3.3-MariaDB\tON\t1\t1002\t/run/mysqld/mysqld.sock\n"), nil
			}
		}
		return nil, nil
	}
	if name != "/usr/bin/mariadb-backup" {
		return nil, errors.New("unexpected executable")
	}
	if slices.Contains(args, "--version") {
		return []byte("mariadb-backup based on MariaDB server 12.3.3-MariaDB\n"), nil
	}
	directory := ""
	for _, argument := range args {
		if strings.HasPrefix(argument, "--target-dir=") {
			directory = strings.TrimPrefix(argument, "--target-dir=")
		}
	}
	if slices.Contains(args, "--backup") {
		if err := os.WriteFile(filepath.Join(directory, "ibdata1"), []byte("database pages"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(directory, "mariadb_backup_checkpoints"), []byte("backup_type = full-backuped\n"), 0o600); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(filepath.Join(directory, "mariadb_backup_slave_info"), []byte("SET GLOBAL gtid_slave_pos='1-2-11';\n"), 0o600)
	}
	if slices.Contains(args, "--prepare") {
		return nil, os.WriteFile(filepath.Join(directory, "mariadb_backup_checkpoints"), []byte("backup_type = log-applied\n"), 0o600)
	}
	return nil, errors.New("unexpected mariadb-backup operation")
}

func TestExecuteMariaDBRunStagesAndDeliversCompletedSet(t *testing.T) {
	root := t.TempDir()
	paths := runPaths{
		work:  makeDirectory(t, root, "work"),
		spool: makeDirectory(t, root, "spool"),
		locks: makeDirectory(t, root, "locks"),
	}
	destination := makeDirectory(t, root, "destination")
	credentials := filepath.Join(root, "client.cnf")
	if err := os.WriteFile(credentials, []byte("[client]\nuser=backup\npassword=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	level := 3
	target := config.Target{
		Driver:      "mariadb",
		Credentials: config.FileReference{File: credentials},
		Source: config.MariaDBSource{
			Socket: "/run/mysqld/mysqld.sock",
			Replica: config.MariaDBReplicaGate{
				Required: true, SourceHost: "fd00::10", SourcePort: 3306,
				SourceUser: "backup_replication", RequireGTID: true, MaxLag: "5m",
			},
		},
		Capture:     config.MariaDBCapture{Prepare: true, SafeReplicaBackup: true, UseMemory: "512M"},
		Compression: &config.Compression{Driver: "zstd", Level: level},
		Destinations: map[string]config.Destination{
			"local": {Driver: "local", Path: destination},
		},
	}
	runner := &pipelineRunner{}
	set, err := executeMariaDBRun(context.Background(), "test", "production-mariadb", target, paths, mariadb.NewCapturerWithRunner(runner))
	if err != nil {
		t.Fatalf("executeMariaDBRun() error = %v", err)
	}
	if set.Manifest.Artifact.Filename != "payload.tar.zst" || set.Manifest.Artifact.SizeBytes == 0 {
		t.Fatalf("artifact = %#v", set.Manifest.Artifact)
	}
	if set.Manifest.Source.Replication["gtid"] != "1-2-11" {
		t.Fatalf("replication metadata = %#v", set.Manifest.Source.Replication)
	}
	store, err := localstore.New(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Verify(context.Background(), set.RelativePath); err != nil {
		t.Fatalf("Verify(delivered) error = %v", err)
	}
	workEntries, err := os.ReadDir(paths.work)
	if err != nil || len(workEntries) != 0 {
		t.Fatalf("work directory was not cleaned: entries=%v error=%v", workEntries, err)
	}
	if runner.calls == 0 {
		t.Fatal("native capture commands were not called")
	}
}

func TestExecuteMariaDBRunPreflightsS3DestinationBeforeCapture(t *testing.T) {
	runner := &pipelineRunner{}
	target := config.Target{
		Destinations: map[string]config.Destination{
			"offsite": {Driver: "s3"},
		},
	}
	_, err := executeMariaDBRun(
		context.Background(),
		"test",
		"production-mariadb",
		target,
		runPaths{},
		mariadb.NewCapturerWithRunner(runner),
	)
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("executeMariaDBRun(S3) error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("capture started before destination preflight: calls=%d", runner.calls)
	}
}

type cliMongoProbe struct{}

func (cliMongoProbe) Inspect(_ context.Context, _ config.Target, _ mongodb.Credentials) (*mongodb.Topology, error) {
	return &mongodb.Topology{
		ServerVersion: "8.3.10", SetName: "example-production", Member: "backup:27017",
		State: "SECONDARY", Hidden: true, Lag: time.Second, Optime: time.Now().UTC(), ConfigVersion: 4,
	}, nil
}

type cliMongoVersionRunner struct{}

func (cliMongoVersionRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return []byte("mongodump version: 100.18.0\n"), nil
}

type cliMongoArchiveRunner struct{}

func (cliMongoArchiveRunner) CaptureArchive(_ context.Context, _ string, args ...string) error {
	for _, argument := range args {
		if strings.HasPrefix(argument, "--archive=") {
			return os.WriteFile(strings.TrimPrefix(argument, "--archive="), []byte("full archive and oplog"), 0o600)
		}
	}
	return errors.New("archive argument missing")
}

func TestExecuteMongoDBRunStagesDeliversAndMaterializesArchive(t *testing.T) {
	root := t.TempDir()
	paths := runPaths{work: makeDirectory(t, root, "work"), spool: makeDirectory(t, root, "spool"), locks: makeDirectory(t, root, "locks")}
	destination := makeDirectory(t, root, "destination")
	credentials := filepath.Join(root, "mongodb.yml")
	if err := os.WriteFile(credentials, []byte("password: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	level := 3
	target := config.Target{
		Driver: "mongodb", Credentials: config.FileReference{File: credentials},
		Source: config.MariaDBSource{
			Host: "127.0.0.1", Port: 27017, Username: "savetoa_backup", AuthenticationDatabase: "admin",
			Replica: config.MariaDBReplicaGate{Required: true, SetName: "example-production", RequireSecondary: true,
				RequireHidden: true, RequireNonVoting: true, RequirePriorityZero: true, MaxLag: "5m"},
		},
		Capture: config.MariaDBCapture{Full: true, Oplog: true}, Compression: &config.Compression{Driver: "zstd", Level: level},
		Destinations: map[string]config.Destination{"local": {Driver: "local", Path: destination}},
	}
	doctor := mongodb.NewDoctorWithDependencies(cliMongoProbe{}, cliMongoVersionRunner{})
	capturer := mongodb.NewCapturerWithDependencies(doctor, cliMongoArchiveRunner{})
	set, err := executeMongoDBRun(context.Background(), "test", "production-mongodb", target, paths, capturer)
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "mongodb" || set.Manifest.Source.Replication["set_name"] != "example-production" {
		t.Fatalf("manifest = %#v", set.Manifest)
	}
	restoreDir := filepath.Join(root, "restore")
	if _, err := restore.Materialize(context.Background(), restore.Options{SourceRoot: destination, BackupID: set.Manifest.BackupID, TargetDir: restoreDir}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restoreDir, "dump.archive"))
	if err != nil || string(data) != "full archive and oplog" {
		t.Fatalf("materialized archive = %q, error=%v", data, err)
	}
}

func makeDirectory(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
