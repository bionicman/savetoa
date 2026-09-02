package mongodb

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

func TestEvaluateTopologyRequiresSafeBackupMember(t *testing.T) {
	target := mongoTarget(t)
	now := time.Now().UTC()
	hello := helloResult{SetName: "example-production", Secondary: true, Me: "backup:27017"}
	status := statusResult{Set: "example-production", Date: now}
	status.Members = append(status.Members,
		struct {
			Name       string    `bson:"name"`
			Health     float64   `bson:"health"`
			StateStr   string    `bson:"stateStr"`
			Self       bool      `bson:"self"`
			OptimeDate time.Time `bson:"optimeDate"`
		}{Name: "primary:27017", Health: 1, StateStr: "PRIMARY", OptimeDate: now},
		struct {
			Name       string    `bson:"name"`
			Health     float64   `bson:"health"`
			StateStr   string    `bson:"stateStr"`
			Self       bool      `bson:"self"`
			OptimeDate time.Time `bson:"optimeDate"`
		}{Name: "backup:27017", Health: 1, StateStr: "SECONDARY", Self: true, OptimeDate: now.Add(-time.Second)},
	)
	var replicaConfig configResult
	replicaConfig.Config.Version = 7
	replicaConfig.Config.Members = append(replicaConfig.Config.Members, struct {
		Host     string  `bson:"host"`
		Hidden   bool    `bson:"hidden"`
		Votes    int     `bson:"votes"`
		Priority float64 `bson:"priority"`
	}{Host: "backup:27017", Hidden: true, Votes: 0, Priority: 0})
	topology, err := evaluateTopology(target, hello, status, replicaConfig)
	if err != nil {
		t.Fatal(err)
	}
	if topology.Lag != time.Second || topology.ConfigVersion != 7 {
		t.Fatalf("topology = %#v", topology)
	}
	replicaConfig.Config.Members[0].Votes = 1
	if _, err := evaluateTopology(target, hello, status, replicaConfig); err == nil {
		t.Fatal("voting backup member passed health gate")
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

type fakeProbe struct{ topology Topology }

func (probe fakeProbe) Inspect(_ context.Context, _ config.Target, credentials Credentials) (*Topology, error) {
	if credentials.Password != "secret" {
		return nil, errors.New("wrong password")
	}
	result := probe.topology
	return &result, nil
}

type fakeRunner struct{}

func (fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != dumpPath || !slices.Equal(args, []string{"--version"}) {
		return nil, errors.New("unexpected command")
	}
	return []byte("mongodump version: 100.18.0\n"), nil
}

type fakeArchiveRunner struct{ password string }

func (runner fakeArchiveRunner) CaptureArchive(_ context.Context, name string, args ...string) error {
	if name != dumpPath {
		return errors.New("unexpected executable")
	}
	archivePath := ""
	hasOplog := false
	for _, argument := range args {
		if strings.Contains(argument, runner.password) || strings.HasPrefix(argument, "--db") || strings.HasPrefix(argument, "--collection") {
			return errors.New("unsafe mongodump argument")
		}
		if strings.HasPrefix(argument, "--archive=") {
			archivePath = strings.TrimPrefix(argument, "--archive=")
		}
		if argument == "--oplog" {
			hasOplog = true
		}
	}
	if archivePath == "" || !hasOplog {
		return errors.New("full archive or oplog argument missing")
	}
	return os.WriteFile(archivePath, []byte("mongo archive with oplog"), 0o600)
}

func TestCaptureCreatesFullOplogArchiveAndCleansUp(t *testing.T) {
	target := mongoTarget(t)
	doctor := NewDoctorWithDependencies(fakeProbe{topology: Topology{
		ServerVersion: "8.3.2", SetName: "example-production", Member: "backup:27017",
		State: "SECONDARY", Hidden: true, Optime: time.Now().UTC(), ConfigVersion: 7,
	}}, fakeRunner{})
	capturer := NewCapturerWithDependencies(doctor, fakeArchiveRunner{password: "secret"})
	capture, err := capturer.Capture(context.Background(), target, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(capture.Path(), "dump.archive")
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}
	directory := capture.Path()
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture directory survived cleanup: %v", err)
	}
}

func mongoTarget(t *testing.T) config.Target {
	t.Helper()
	credentials := filepath.Join(t.TempDir(), "mongodb.yml")
	if err := os.WriteFile(credentials, []byte("password: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Target{
		Driver: "mongodb", Credentials: config.FileReference{File: credentials},
		Source: config.MariaDBSource{
			Host: "127.0.0.1", Port: 27017, Username: "savetoa_backup", AuthenticationDatabase: "admin",
			Replica: config.MariaDBReplicaGate{Required: true, SetName: "example-production", RequireSecondary: true,
				RequireHidden: true, RequireNonVoting: true, RequirePriorityZero: true, MaxLag: "5m"},
		},
		Capture: config.MariaDBCapture{Full: true, Oplog: true},
	}
}
