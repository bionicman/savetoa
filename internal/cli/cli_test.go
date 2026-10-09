package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/config"
	"github.com/bionicman/savetoa/internal/hook"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
	"github.com/bionicman/savetoa/internal/mariadb"
	"github.com/bionicman/savetoa/internal/mariadbdump"
	"github.com/bionicman/savetoa/internal/mongodb"
	"github.com/bionicman/savetoa/internal/postgresql"
	redisdriver "github.com/bionicman/savetoa/internal/redis"
	"github.com/bionicman/savetoa/internal/restore"
	sqlite3driver "github.com/bionicman/savetoa/internal/sqlite3"
	statuspkg "github.com/bionicman/savetoa/internal/status"
	"github.com/bionicman/savetoa/internal/tardriver"
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

	code := Run([]string{"run-group"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("unimplemented backup command returned success")
	}
	if !strings.Contains(stderr.String(), "not implemented") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}

func TestListReportsCompletedSetsAcrossRepositoriesAsJSON(t *testing.T) {
	root := t.TempDir()
	spoolRoot := makeDirectory(t, root, "spool")
	destinationRoot := makeDirectory(t, root, "destination")
	locksRoot := makeDirectory(t, root, "locks")
	completed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, storeRoot := range []string{spoolRoot, destinationRoot} {
		commitRetentionSet(t, storeRoot, "20260903t120000z-listed", completed)
	}
	configPath := writeRepositoryStatusConfig(t, root, destinationRoot)
	previousPaths := defaultRunPaths
	defaultRunPaths = runPaths{work: previousPaths.work, spool: spoolRoot, locks: locksRoot}
	t.Cleanup(func() { defaultRunPaths = previousPaths })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--config", configPath, "list", "--format", "json", "database"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stderr=%q", code, stderr.String())
	}
	var report statuspkg.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, stdout.String())
	}
	if report.SchemaVersion != 1 || report.Status != "complete" || report.LatestBackupID != "20260903t120000z-listed" {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Repositories) != 2 || len(report.Backups) != 1 || len(report.Backups[0].Repositories) != 2 {
		t.Fatalf("repository report = %#v", report)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--config", configPath, "status", "--max-age", "24h", "database"}, &stdout, &stderr); code != 0 {
		t.Fatalf("complete status code=%d stderr=%q output=%q", code, stderr.String(), stdout.String())
	}
}

func TestStatusFailsForStaleAndEmptyRepositories(t *testing.T) {
	for _, test := range []struct {
		name       string
		commitSets bool
		wantStatus string
	}{
		{name: "stale", commitSets: true, wantStatus: "stale"},
		{name: "empty", wantStatus: "empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			spoolRoot := makeDirectory(t, root, "spool")
			destinationRoot := makeDirectory(t, root, "destination")
			locksRoot := makeDirectory(t, root, "locks")
			if test.commitSets {
				completed := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
				for _, storeRoot := range []string{spoolRoot, destinationRoot} {
					commitRetentionSet(t, storeRoot, "20260901t120000z-stale", completed)
				}
			}
			configPath := writeRepositoryStatusConfig(t, root, destinationRoot)
			previousPaths := defaultRunPaths
			defaultRunPaths = runPaths{work: previousPaths.work, spool: spoolRoot, locks: locksRoot}
			t.Cleanup(func() { defaultRunPaths = previousPaths })

			var stdout, stderr bytes.Buffer
			code := Run([]string{"--config", configPath, "status", "--format", "json", "--max-age", "24h", "database"}, &stdout, &stderr)
			if code != 1 || stderr.Len() != 0 {
				t.Fatalf("status code=%d stderr=%q", code, stderr.String())
			}
			var report statuspkg.Report
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", report.Status, test.wantStatus)
			}
		})
	}
}

func TestStatusEmitsFailureHookEvent(t *testing.T) {
	root := t.TempDir()
	spoolRoot := makeDirectory(t, root, "spool")
	destinationRoot := makeDirectory(t, root, "destination")
	locksRoot := makeDirectory(t, root, "locks")
	configPath := writeRepositoryStatusConfig(t, root, destinationRoot)
	hookRoot := makeDirectory(t, root, "hooks")
	hookDirectory := filepath.Join(hookRoot, "status", "failure.d")
	if err := os.MkdirAll(hookDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(root, "event.json")
	script := "#!/bin/sh\n/bin/cat > '" + strings.ReplaceAll(eventPath, "'", "'\\''") + "'\n"
	if err := os.WriteFile(filepath.Join(hookDirectory, "capture"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previousPaths := defaultRunPaths
	previousRunner := lifecycleHookRunner
	defaultRunPaths = runPaths{work: previousPaths.work, spool: spoolRoot, locks: locksRoot}
	lifecycleHookRunner = hook.Runner{Root: hookRoot, Timeout: 30 * time.Second, RequiredUID: uint32(os.Getuid())}
	t.Cleanup(func() {
		defaultRunPaths = previousPaths
		lifecycleHookRunner = previousRunner
	})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", configPath, "status", "--format", "json", "database"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("status code=%d stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	var event hook.Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Action != "status" || event.Outcome != "failure" || event.ExitCode != 1 || event.Environment != "test" || event.Target != "database" {
		t.Fatalf("event = %#v", event)
	}
}

func TestHookFailureDoesNotChangeSuccessfulStatus(t *testing.T) {
	root := t.TempDir()
	spoolRoot := makeDirectory(t, root, "spool")
	destinationRoot := makeDirectory(t, root, "destination")
	locksRoot := makeDirectory(t, root, "locks")
	completed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, storeRoot := range []string{spoolRoot, destinationRoot} {
		commitRetentionSet(t, storeRoot, "20260903t120000z-hooked", completed)
	}
	configPath := writeRepositoryStatusConfig(t, root, destinationRoot)
	hookRoot := makeDirectory(t, root, "hooks")
	hookDirectory := filepath.Join(hookRoot, "status", "success.d")
	if err := os.MkdirAll(hookDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDirectory, "broken"), []byte("#!/bin/sh\necho private-webhook-response >&2\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousPaths := defaultRunPaths
	previousRunner := lifecycleHookRunner
	defaultRunPaths = runPaths{work: previousPaths.work, spool: spoolRoot, locks: locksRoot}
	lifecycleHookRunner = hook.Runner{Root: hookRoot, Timeout: 30 * time.Second, RequiredUID: uint32(os.Getuid())}
	t.Cleanup(func() {
		defaultRunPaths = previousPaths
		lifecycleHookRunner = previousRunner
	})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--config", configPath, "status", "database"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "hook action=status outcome=success failed") || strings.Contains(stderr.String(), "private-webhook-response") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func writeRepositoryStatusConfig(t *testing.T, root, destinationRoot string) string {
	t.Helper()
	configPath := filepath.Join(root, "config.yml")
	configuration := `config_version: 1
environment: test
targets:
  database:
    driver: mariadb
    credentials:
      file: /etc/savetoa/credentials.d/database.cnf
    source:
      socket: /run/mysqld/mysqld.sock
      replica:
        required: true
        source_host: primary.internal
        source_port: 3306
        source_user: replication
        require_gtid: true
        max_lag: 5m
    capture:
      prepare: true
      safe_replica_backup: true
      use_memory: 512M
    destinations:
      local:
        driver: local
        path: ` + destinationRoot + `
groups: {}
`
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestPruneAppliesRetentionToSpoolAndLocalDestination(t *testing.T) {
	root := t.TempDir()
	spoolRoot := makeDirectory(t, root, "spool")
	destinationRoot := makeDirectory(t, root, "destination")
	locksRoot := makeDirectory(t, root, "locks")
	for _, storeRoot := range []string{spoolRoot, destinationRoot} {
		commitRetentionSet(t, storeRoot, "20260901t120000z-old", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
		commitRetentionSet(t, storeRoot, "20260902t120000z-new", time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	}
	configPath := filepath.Join(root, "config.yml")
	configuration := `config_version: 1
environment: test
targets:
  database:
    driver: mariadb
    credentials:
      file: /etc/savetoa/credentials.d/database.cnf
    source:
      socket: /run/mysqld/mysqld.sock
      replica:
        required: true
        source_host: primary.internal
        source_port: 3306
        source_user: replication
        require_gtid: true
        max_lag: 5m
    capture:
      prepare: true
      safe_replica_backup: true
      use_memory: 512M
    destinations:
      local:
        driver: local
        path: ` + destinationRoot + `
    retention:
      keep_daily: 1
      keep_weekly: 0
      keep_monthly: 0
groups: {}
`
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	previousPaths := defaultRunPaths
	defaultRunPaths = runPaths{work: previousPaths.work, spool: spoolRoot, locks: locksRoot}
	t.Cleanup(func() { defaultRunPaths = previousPaths })
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--config", configPath, "prune", "database"}, &stdout, &stderr); code != 0 {
		t.Fatalf("prune code=%d stderr=%q", code, stderr.String())
	}
	if strings.Count(stdout.String(), "pruned=1") != 2 {
		t.Fatalf("prune output = %q", stdout.String())
	}
	for _, storeRoot := range []string{spoolRoot, destinationRoot} {
		store, err := localstore.New(storeRoot)
		if err != nil {
			t.Fatal(err)
		}
		sets, err := store.ListCompleted("test", "database")
		_ = store.Close()
		if err != nil || len(sets) != 1 || sets[0].Manifest.BackupID != "20260902t120000z-new" {
			t.Fatalf("sets in %s = %#v, error=%v", storeRoot, sets, err)
		}
	}
}

func commitRetentionSet(t *testing.T, root, backupID string, started time.Time) {
	t.Helper()
	store, err := localstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value := manifest.Manifest{
		FormatVersion: manifest.FormatVersion, BackupID: backupID, Target: "database", CaptureDriver: "mariadb",
		StartedAt: started, CompletedAt: started.Add(time.Minute),
		Tool:     manifest.Tool{Name: "mariadb-backup", Version: "12.3.3"},
		Source:   manifest.Source{ServerVersion: "12.3.3", Replication: map[string]string{"gtid": "0-1-2"}},
		Artifact: manifest.Artifact{Filename: "payload.tar"}, Transformations: []manifest.Transformation{},
	}
	if _, err := store.Commit(context.Background(), "test", value, strings.NewReader("payload")); err != nil {
		t.Fatal(err)
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

type cliRedisClient struct {
	state redisdriver.State
}

func (client *cliRedisClient) Inspect(_ context.Context, _ config.Target, _ redisdriver.Credentials) (*redisdriver.State, error) {
	result := client.state
	return &result, nil
}

func (client *cliRedisClient) BGSAVE(_ context.Context, _ config.Target, _ redisdriver.Credentials) error {
	client.state.LastSave++
	return nil
}

type cliRedisVersionRunner struct{}

func (cliRedisVersionRunner) Version(context.Context) (string, error) { return "8.0.5", nil }

func TestExecuteRedisRunStagesDeliversAndMaterializesRDB(t *testing.T) {
	root := t.TempDir()
	paths := runPaths{work: makeDirectory(t, root, "work"), spool: makeDirectory(t, root, "spool"), locks: makeDirectory(t, root, "locks")}
	destination := makeDirectory(t, root, "destination")
	credentials := filepath.Join(root, "redis.yml")
	if err := os.WriteFile(credentials, []byte("password: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rdb := filepath.Join(root, "dump.rdb")
	if err := os.WriteFile(rdb, []byte("REDIS0012 pipeline snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := config.Target{
		Driver: "redis", Credentials: config.FileReference{File: credentials},
		Source: config.MariaDBSource{
			Host: "127.0.0.1", Port: 6379, Username: "savetoa_backup", RDBFile: rdb,
			Replica: config.MariaDBReplicaGate{
				Required: true, SourceHost: "redis-primary.internal", SourcePort: 6379,
				RequireReadOnly: true, RequirePriorityZero: true, MaxLag: "5s",
			},
		},
		Capture:      config.MariaDBCapture{BGSAVE: true, Schedule: true, MaxWait: "10m"},
		Compression:  &config.Compression{Driver: "zstd", Level: 3},
		Destinations: map[string]config.Destination{"local": {Driver: "local", Path: destination}},
	}
	client := &cliRedisClient{state: redisdriver.State{
		ServerVersion: "8.0.5", Role: "slave", MasterHost: "redis-primary.internal", MasterPort: 6379,
		MasterLinkStatus: "up", Lag: time.Second, ReplicationID: "abc", ReplicationOffset: 99,
		ReadOnly: true, LastSave: time.Now().Add(-2 * time.Second).Unix(),
		LastBGSAVEStatus: "ok", RDBFile: rdb,
	}}
	doctor := redisdriver.NewDoctorWithDependencies(client, cliRedisVersionRunner{})
	capturer := redisdriver.NewCapturerWithDependencies(doctor, client)
	set, err := executeRedisRun(context.Background(), "test", "production-redis", target, paths, capturer)
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "redis" || set.Manifest.Source.Replication["replication_offset"] != "99" {
		t.Fatalf("manifest = %#v", set.Manifest)
	}
	restoreDir := filepath.Join(root, "restore-redis")
	if _, err := restore.Materialize(context.Background(), restore.Options{
		SourceRoot: destination, BackupID: set.Manifest.BackupID, TargetDir: restoreDir,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restoreDir, "dump.rdb"))
	if err != nil || string(data) != "REDIS0012 pipeline snapshot" {
		t.Fatalf("materialized RDB = %q, error=%v", data, err)
	}
}

type cliSQLite3Runner struct {
	payload []byte
	calls   int
}

func (runner *cliSQLite3Runner) Version(context.Context) (string, error) {
	runner.calls++
	return "3.46.1", nil
}

func (runner *cliSQLite3Runner) QuickCheck(context.Context, string) error {
	runner.calls++
	return nil
}

func (runner *cliSQLite3Runner) Backup(_ context.Context, _ string, directory string) error {
	runner.calls++
	return os.WriteFile(filepath.Join(directory, "database.sqlite3"), runner.payload, 0o600)
}

func TestExecuteSQLite3RunStagesDeliversAndMaterializesDatabase(t *testing.T) {
	root := t.TempDir()
	paths := runPaths{work: makeDirectory(t, root, "work"), spool: makeDirectory(t, root, "spool"), locks: makeDirectory(t, root, "locks")}
	destination := makeDirectory(t, root, "destination")
	source := filepath.Join(root, "application.sqlite3")
	if err := os.WriteFile(source, []byte("source database"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("consistent SQLite snapshot")
	target := config.Target{
		Driver:       "sqlite3",
		Source:       config.MariaDBSource{Path: source},
		Compression:  &config.Compression{Driver: "zstd", Level: 3},
		Destinations: map[string]config.Destination{"local": {Driver: "local", Path: destination}},
	}
	runner := &cliSQLite3Runner{payload: payload}
	set, err := executeSQLite3Run(context.Background(), "test", "application-sqlite", target, paths, sqlite3driver.NewCapturerWithRunner(runner))
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "sqlite3" || set.Manifest.Tool.Name != "sqlite3" ||
		set.Manifest.Source.ServerVersion != "3.46.1" || len(set.Manifest.Source.Replication) != 0 {
		t.Fatalf("manifest = %#v", set.Manifest)
	}
	restoreDir := filepath.Join(root, "restore-sqlite")
	if _, err := restore.Materialize(context.Background(), restore.Options{
		SourceRoot: destination, BackupID: set.Manifest.BackupID, TargetDir: restoreDir,
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(filepath.Join(restoreDir, "database.sqlite3"))
	if err != nil || string(restored) != string(payload) {
		t.Fatalf("materialized SQLite database = %q, error=%v", restored, err)
	}
	if runner.calls == 0 {
		t.Fatal("SQLite capture runner was not called")
	}
}

type cliPostgreSQLRunner struct{}

func (cliPostgreSQLRunner) Version(context.Context, string) (string, error) { return "18.1", nil }
func (cliPostgreSQLRunner) Probe(context.Context, config.Target) (string, bool, error) {
	return "18.1", true, nil
}
func (cliPostgreSQLRunner) Capture(_ context.Context, target config.Target, directory string) error {
	name := "database.dump"
	if target.Driver == "postgresql-base" {
		name = "base.tar"
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte("captured PostgreSQL"), 0o600); err != nil {
		return err
	}
	if target.Driver == "postgresql-base" {
		if err := os.WriteFile(filepath.Join(directory, "pg_wal.tar"), []byte("wal"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(directory, "backup_manifest"), []byte("manifest"), 0o600)
	}
	return nil
}
func (cliPostgreSQLRunner) Verify(context.Context, config.Target, string) error { return nil }

type cliMariaDBDumpRunner struct{}

func (cliMariaDBDumpRunner) Version(context.Context) (string, error) { return "11.8.6", nil }
func (cliMariaDBDumpRunner) Probe(context.Context, config.Target) (string, error) {
	return "11.8.6", nil
}
func (cliMariaDBDumpRunner) Capture(_ context.Context, _ config.Target, path string) error {
	return os.WriteFile(path, []byte("CREATE TABLE smoke (id INT);\n-- Dump completed\n"), 0o600)
}

func TestExecuteMariaDBDumpRunStagesAndMaterializes(t *testing.T) {
	root := t.TempDir()
	paths := runPaths{work: makeDirectory(t, root, "work"), spool: makeDirectory(t, root, "spool"), locks: makeDirectory(t, root, "locks")}
	destination := makeDirectory(t, root, "destination")
	credential := filepath.Join(root, "mariadb.cnf")
	if err := os.WriteFile(credential, []byte("[client]\nuser=backup\npassword=unused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := config.Target{
		Driver:      "mariadb-dump",
		Credentials: config.FileReference{File: credential},
		Source:      config.MariaDBSource{Socket: "/run/mysqld/mysqld.sock", Database: "mailserver"},
		Compression: &config.Compression{Driver: "zstd", Level: 3},
		Destinations: map[string]config.Destination{
			"local": {Driver: "local", Path: destination},
		},
	}
	set, err := executeMariaDBDumpRun(context.Background(), "test", "mailserver", target, paths, mariadbdump.NewCapturerWithRunner(cliMariaDBDumpRunner{}))
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "mariadb-dump" || set.Manifest.Tool.Name != "mariadb-dump" ||
		set.Manifest.Source.ServerVersion != "11.8.6" || set.Manifest.Source.Database != "mailserver" ||
		len(set.Manifest.Source.Replication) != 0 {
		t.Fatalf("manifest=%#v", set.Manifest)
	}
	restored := filepath.Join(root, "restored")
	if _, err := restore.Materialize(context.Background(), restore.Options{SourceRoot: destination, BackupID: set.Manifest.BackupID, TargetDir: restored}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restored, "database.sql"))
	if err != nil || !strings.HasSuffix(string(data), "\n-- Dump completed\n") {
		t.Fatalf("materialized dump=%q error=%v", data, err)
	}
}

func TestExecutePostgreSQLRunStagesBothFormats(t *testing.T) {
	for _, driver := range []string{"postgresql-base", "postgresql-dump"} {
		t.Run(driver, func(t *testing.T) {
			root := t.TempDir()
			paths := runPaths{work: makeDirectory(t, root, "work"), spool: makeDirectory(t, root, "spool"), locks: makeDirectory(t, root, "locks")}
			destination := makeDirectory(t, root, "destination")
			passfile := filepath.Join(root, "pgpass")
			if err := os.WriteFile(passfile, []byte("localhost:5432:*:backup:secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := config.Target{Driver: driver, Credentials: config.FileReference{File: passfile},
				Source:       config.MariaDBSource{Host: "localhost", Port: 5432, Username: "backup", Database: "appdb"},
				Destinations: map[string]config.Destination{"local": {Driver: "local", Path: destination}}}
			if driver == "postgresql-base" {
				target.Source.Database = ""
				required := true
				target.Source.RequireStandby = &required
			}
			set, err := executePostgreSQLRun(context.Background(), "test", "postgresql", target, paths, postgresql.NewCapturerWithRunner(cliPostgreSQLRunner{}))
			if err != nil {
				t.Fatal(err)
			}
			if set.Manifest.CaptureDriver != driver || set.Manifest.Tool.Version != "18.1" {
				t.Fatalf("manifest=%#v", set.Manifest)
			}
			if set.Manifest.Source.Database != target.Source.Database {
				t.Fatalf("database provenance=%q, want %q", set.Manifest.Source.Database, target.Source.Database)
			}
			restored := filepath.Join(root, "restored")
			if _, err := restore.Materialize(context.Background(), restore.Options{SourceRoot: destination, BackupID: set.Manifest.BackupID, TargetDir: restored}); err != nil {
				t.Fatal(err)
			}
			name := "database.dump"
			if driver == "postgresql-base" {
				name = "base.tar"
			}
			if data, err := os.ReadFile(filepath.Join(restored, name)); err != nil || string(data) != "captured PostgreSQL" {
				t.Fatalf("restored %s: %q %v", name, data, err)
			}
		})
	}
}

type cliTarRunner struct {
	payload []byte
}

func (runner cliTarRunner) Version(context.Context) (string, error) { return "1.35", nil }

func (runner cliTarRunner) Create(context.Context, []string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(runner.payload)), nil
}

func TestExecuteTarRunStagesDeliversAndMaterializesPaths(t *testing.T) {
	root := t.TempDir()
	paths := runPaths{work: makeDirectory(t, root, "work"), spool: makeDirectory(t, root, "spool"), locks: makeDirectory(t, root, "locks")}
	destination := makeDirectory(t, root, "destination")
	source := makeDirectory(t, root, "source")
	if err := os.WriteFile(filepath.Join(source, "account.conf"), []byte("source probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	data := []byte("captured ACME state")
	if err := writer.WriteHeader(&tar.Header{Name: "var/lib/acme.sh/account.conf", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	target := config.Target{
		Driver: "tar", Source: config.MariaDBSource{Paths: []string{source}},
		Compression:  &config.Compression{Driver: "zstd", Level: 3},
		Destinations: map[string]config.Destination{"local": {Driver: "local", Path: destination}},
	}
	capturer := tardriver.NewCapturerWithRunner(cliTarRunner{payload: payload.Bytes()})
	set, err := executeTarRun(context.Background(), "test", "production-acme", target, paths, capturer)
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "tar" || set.Manifest.Tool.Name != "tar" || set.Manifest.Artifact.Filename != "payload.tar.zst" {
		t.Fatalf("manifest = %#v", set.Manifest)
	}
	restoreDir := filepath.Join(root, "restore-tar")
	if _, err := restore.Materialize(context.Background(), restore.Options{
		SourceRoot: destination, BackupID: set.Manifest.BackupID, TargetDir: restoreDir,
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(filepath.Join(restoreDir, "var", "lib", "acme.sh", "account.conf"))
	if err != nil || string(restored) != string(data) {
		t.Fatalf("materialized tar file = %q, error=%v", restored, err)
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
