package redis

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

type fakeClient struct {
	states []State
	index  int
	called bool
}

func (client *fakeClient) Inspect(_ context.Context, _ config.Target, credentials Credentials) (*State, error) {
	if credentials.Password != "secret" || len(client.states) == 0 {
		return nil, errors.New("unexpected inspect")
	}
	index := client.index
	if index >= len(client.states) {
		index = len(client.states) - 1
	} else {
		client.index++
	}
	result := client.states[index]
	return &result, nil
}

func (client *fakeClient) BGSAVE(_ context.Context, _ config.Target, credentials Credentials) error {
	if credentials.Password != "secret" {
		return errors.New("wrong credentials")
	}
	client.called = true
	return nil
}

type fakeVersion struct{}

func (fakeVersion) Version(context.Context) (string, error) { return "8.0.5", nil }

func TestEvaluateStateRequiresSafeReplica(t *testing.T) {
	target := redisTarget(t)
	state := healthyState(target)
	if err := evaluateState(target, &state); err != nil {
		t.Fatal(err)
	}
	state.Priority = 1
	if err := evaluateState(target, &state); err == nil {
		t.Fatal("promotable Redis replica passed health gate")
	}
	state = healthyState(target)
	state.ReadOnly = false
	if err := evaluateState(target, &state); err == nil {
		t.Fatal("writable Redis replica passed health gate")
	}
}

func TestCaptureCreatesFreshRDBAndCleansUp(t *testing.T) {
	target := redisTarget(t)
	baseline := time.Now().Add(-2 * time.Second).Unix()
	if err := os.WriteFile(target.Source.RDBFile, []byte("REDIS0012 fresh snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := healthyState(target)
	state.LastSave = baseline
	completed := state
	completed.LastSave = baseline + 1
	client := &fakeClient{states: []State{state, completed, completed}}
	doctor := NewDoctorWithDependencies(client, fakeVersion{})
	capturer := NewCapturerWithDependencies(doctor, client)
	capture, err := capturer.Capture(context.Background(), target, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !client.called {
		t.Fatal("BGSAVE was not requested")
	}
	if data, err := os.ReadFile(filepath.Join(capture.Path(), "dump.rdb")); err != nil || string(data) != "REDIS0012 fresh snapshot" {
		t.Fatalf("captured RDB = %q, %v", data, err)
	}
	directory := capture.Path()
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture directory survived cleanup: %v", err)
	}
}

func TestCredentialsFailClosedWithoutLeakingPassword(t *testing.T) {
	secret := "do-not-leak"
	for _, input := range []string{
		"password: " + secret + "\nunknown: true\n",
		"password: " + secret + "\n---\n{}\n",
		"password: |\n  " + secret + "\n  second\n",
	} {
		_, err := ReadCredentials(strings.NewReader(input))
		if err == nil {
			t.Fatal("ReadCredentials(invalid) succeeded")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked password: %v", err)
		}
	}
}

func TestParseInfoRejectsDuplicateFields(t *testing.T) {
	if _, err := parseInfo([]byte("role:slave\nrole:master\n")); err == nil {
		t.Fatal("duplicate Redis INFO field was accepted")
	}
}

func TestRedisCLIArgumentsUseSupportedFlagsAndExcludePassword(t *testing.T) {
	target := redisTarget(t)
	arguments := clientArguments(target, "INFO", "replication")
	want := []string{
		"--no-auth-warning", "--raw", "-h", "127.0.0.1", "-p", "6379",
		"--user", "savetoa_backup", "INFO", "replication",
	}
	if !slices.Equal(arguments, want) {
		t.Fatalf("clientArguments() = %#v, want %#v", arguments, want)
	}
	if strings.Contains(strings.Join(arguments, " "), "secret") {
		t.Fatal("Redis password entered argv")
	}
}

func TestCopyRDBRejectsStaleAndSymlinkSources(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "dump.rdb")
	if err := os.WriteFile(source, []byte("REDIS0012 stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyRDB(source, filepath.Join(directory, "captured.rdb"), time.Now().Add(time.Hour).Unix()); err == nil {
		t.Fatal("stale RDB source was accepted")
	}
	link := filepath.Join(directory, "link.rdb")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := copyRDB(link, filepath.Join(directory, "linked.rdb"), 1); err == nil {
		t.Fatal("symlink RDB source was accepted")
	}
}

func redisTarget(t *testing.T) config.Target {
	t.Helper()
	directory := t.TempDir()
	credentials := filepath.Join(directory, "redis.yml")
	if err := os.WriteFile(credentials, []byte("password: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Target{
		Driver: "redis", Credentials: config.FileReference{File: credentials},
		Source: config.MariaDBSource{
			Host: "127.0.0.1", Port: 6379, Username: "savetoa_backup",
			RDBFile: filepath.Join(directory, "dump.rdb"),
			Replica: config.MariaDBReplicaGate{
				Required: true, SourceHost: "redis-primary.internal", SourcePort: 6379,
				RequireReadOnly: true, RequirePriorityZero: true, MaxLag: "5s",
			},
		},
		Capture: config.MariaDBCapture{BGSAVE: true, Schedule: true, MaxWait: "10m"},
	}
}

func healthyState(target config.Target) State {
	return State{
		ServerVersion: "8.0.5", Role: "slave",
		MasterHost: target.Source.Replica.SourceHost, MasterPort: target.Source.Replica.SourcePort,
		MasterLinkStatus: "up", Lag: time.Second, ReplicationID: "abcdef", ReplicationOffset: 42,
		ReadOnly: true, Priority: 0, LastSave: time.Now().Add(-time.Second).Unix(),
		LastBGSAVEStatus: "ok", RDBFile: target.Source.RDBFile,
	}
}
