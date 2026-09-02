package mariadb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bionicman/savetoa/internal/config"
)

type fakeRunner struct {
	calls [][]string
	fail  bool
}

func (runner *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string{name}, args...))
	if runner.fail {
		return nil, errors.New("password=must-never-escape")
	}
	if name == backupPath {
		return []byte("mariadb-backup based on MariaDB server 12.3.3-MariaDB\n"), nil
	}
	if slices.Contains(args, "--vertical") {
		return []byte(`*************************** 1. row ***************************
Master_Host: fd00::10
Master_User: backup_replication
Master_Port: 3306
Slave_IO_Running: Yes
Slave_SQL_Running: Yes
Seconds_Behind_Master: 2
Last_IO_Errno: 0
Last_SQL_Errno: 0
Using_Gtid: Slave_Pos
Gtid_IO_Pos: 1-2-3
`), nil
	}
	return []byte("12.3.3-MariaDB\tON\t1\t1002\t/run/mysqld/mysqld.sock\n"), nil
}

func TestDoctorAcceptsHealthyReplica(t *testing.T) {
	runner := &fakeRunner{}
	target := testTarget(t)
	report, err := NewDoctorWithRunner(runner).Check(context.Background(), target)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.ServerVersion != "12.3.3" || report.GTIDPosition != "1-2-3" {
		t.Fatalf("Check() report = %#v", report)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("command count = %d, want 3", len(runner.calls))
	}
	first := runner.calls[0]
	if first[0] != clientPath || !strings.HasPrefix(first[1], "--defaults-extra-file=") {
		t.Fatalf("first command does not use fixed client and first option file: %#v", first)
	}
	if slices.Contains(runner.calls[1], "--skip-column-names") {
		t.Fatalf("vertical replica query suppressed field names: %#v", runner.calls[1])
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "must-never-escape") {
			t.Fatalf("secret reached argv: %#v", call)
		}
	}
}

func TestDoctorRedactsNativeErrors(t *testing.T) {
	runner := &fakeRunner{fail: true}
	_, err := NewDoctorWithRunner(runner).Check(context.Background(), testTarget(t))
	if err == nil || strings.Contains(err.Error(), "must-never-escape") {
		t.Fatalf("Check() error was not redacted: %v", err)
	}
}

func TestDoctorRejectsUnsafeCredentialFile(t *testing.T) {
	target := testTarget(t)
	if err := os.Chmod(target.Credentials.File, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewDoctorWithRunner(&fakeRunner{}).Check(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestDoctorRejectsReplicaDrift(t *testing.T) {
	target := testTarget(t)
	target.Source.Replica.SourceHost = "wrong-source"
	_, err := NewDoctorWithRunner(&fakeRunner{}).Check(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "source identity") {
		t.Fatalf("Check() error = %v", err)
	}
}

func testTarget(t *testing.T) config.Target {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.cnf")
	if err := os.WriteFile(path, []byte("[client]\nuser=backup\npassword=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Target{
		Driver:      "mariadb",
		Credentials: config.FileReference{File: path},
		Source: config.MariaDBSource{
			Socket: "/run/mysqld/mysqld.sock",
			Replica: config.MariaDBReplicaGate{
				Required: true, SourceHost: "fd00::10", SourcePort: 3306,
				SourceUser: "backup_replication", RequireGTID: true, MaxLag: "5m",
			},
		},
	}
}
