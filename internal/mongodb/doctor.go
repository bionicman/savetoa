// Package mongodb implements MongoDB-specific health and capture operations.
package mongodb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bionicman/savetoa/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

const dumpPath = "/usr/bin/mongodump"

var toolsVersionPattern = regexp.MustCompile(`\b([0-9]+\.[0-9]+\.[0-9]+)\b`)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return output, nil
}

type Topology struct {
	ServerVersion string
	SetName       string
	Member        string
	State         string
	Hidden        bool
	Votes         int
	Priority      float64
	Lag           time.Duration
	Optime        time.Time
	ConfigVersion int
}

type Probe interface {
	Inspect(context.Context, config.Target, Credentials) (*Topology, error)
}

type DriverProbe struct{}

type helloResult struct {
	SetName   string `bson:"setName"`
	Secondary bool   `bson:"secondary"`
	Me        string `bson:"me"`
}

type buildInfoResult struct {
	Version string `bson:"version"`
}

type statusResult struct {
	Set     string    `bson:"set"`
	Date    time.Time `bson:"date"`
	Members []struct {
		Name       string    `bson:"name"`
		Health     float64   `bson:"health"`
		StateStr   string    `bson:"stateStr"`
		Self       bool      `bson:"self"`
		OptimeDate time.Time `bson:"optimeDate"`
	} `bson:"members"`
}

type configResult struct {
	Config struct {
		Version int `bson:"version"`
		Members []struct {
			Host     string  `bson:"host"`
			Hidden   bool    `bson:"hidden"`
			Votes    int     `bson:"votes"`
			Priority float64 `bson:"priority"`
		} `bson:"members"`
	} `bson:"config"`
}

func (DriverProbe) Inspect(ctx context.Context, target config.Target, credentials Credentials) (*Topology, error) {
	host := net.JoinHostPort(target.Source.Host, fmt.Sprintf("%d", target.Source.Port))
	client, err := mongo.Connect(options.Client().
		SetHosts([]string{host}).
		SetDirect(true).
		SetReplicaSet(target.Source.Replica.SetName).
		SetReadPreference(readpref.Secondary()).
		SetRetryWrites(false).
		SetServerSelectionTimeout(10 * time.Second).
		SetAuth(options.Credential{
			AuthSource: target.Source.AuthenticationDatabase,
			Username:   target.Source.Username,
			Password:   credentials.Password,
		}))
	if err != nil {
		return nil, errors.New("connect to MongoDB")
	}
	defer func() {
		disconnectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(disconnectCtx)
	}()
	admin := client.Database("admin")
	var hello helloResult
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return nil, errors.New("inspect MongoDB member identity")
	}
	var buildInfo buildInfoResult
	if err := admin.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&buildInfo); err != nil {
		return nil, errors.New("inspect MongoDB server version")
	}
	var status statusResult
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status); err != nil {
		return nil, errors.New("inspect MongoDB replica status")
	}
	var replicaConfig configResult
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&replicaConfig); err != nil {
		return nil, errors.New("inspect MongoDB replica configuration")
	}
	result, err := evaluateTopology(target, hello, status, replicaConfig)
	if err != nil {
		return nil, err
	}
	result.ServerVersion = buildInfo.Version
	return result, nil
}

func evaluateTopology(target config.Target, hello helloResult, status statusResult, replicaConfig configResult) (*Topology, error) {
	if hello.SetName != target.Source.Replica.SetName || status.Set != target.Source.Replica.SetName {
		return nil, errors.New("MongoDB replica-set identity does not match configuration")
	}
	if !hello.Secondary || hello.Me == "" {
		return nil, errors.New("MongoDB target is not a secondary replica-set member")
	}
	var selfOptime, primaryOptime time.Time
	selfHealthy := false
	for _, member := range status.Members {
		if member.StateStr == "PRIMARY" && member.Health == 1 {
			primaryOptime = member.OptimeDate
		}
		if member.Self {
			selfHealthy = member.Health == 1 && member.StateStr == "SECONDARY" && member.Name == hello.Me
			selfOptime = member.OptimeDate
		}
	}
	if !selfHealthy || selfOptime.IsZero() || primaryOptime.IsZero() {
		return nil, errors.New("MongoDB replica status lacks a healthy primary and self secondary")
	}
	lag := primaryOptime.Sub(selfOptime)
	if lag < 0 {
		lag = 0
	}
	maxLag, _ := time.ParseDuration(target.Source.Replica.MaxLag)
	if lag > maxLag {
		return nil, fmt.Errorf("MongoDB replication lag %s exceeds configured maximum %s", lag, maxLag)
	}
	var found bool
	var hidden bool
	var votes int
	var priority float64
	for _, member := range replicaConfig.Config.Members {
		if member.Host == hello.Me {
			found, hidden, votes, priority = true, member.Hidden, member.Votes, member.Priority
			break
		}
	}
	if !found || !hidden || votes != 0 || priority != 0 {
		return nil, errors.New("MongoDB backup member must be hidden, non-voting, and priority zero")
	}
	return &Topology{
		SetName: hello.SetName, Member: hello.Me,
		State: "SECONDARY", Hidden: hidden, Votes: votes, Priority: priority,
		Lag: lag, Optime: selfOptime.UTC(), ConfigVersion: replicaConfig.Config.Version,
	}, nil
}

type Doctor struct {
	probe  Probe
	runner Runner
}

type Report struct {
	Topology
	ToolsVersion string
}

func NewDoctor() *Doctor { return &Doctor{probe: DriverProbe{}, runner: CommandRunner{}} }

func NewDoctorWithDependencies(probe Probe, runner Runner) *Doctor {
	return &Doctor{probe: probe, runner: runner}
}

func (doctor *Doctor) Check(ctx context.Context, target config.Target) (*Report, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if doctor == nil || doctor.probe == nil || doctor.runner == nil {
		return nil, errors.New("MongoDB doctor dependencies are required")
	}
	credentials, err := ReadCredentialsFile(target.Credentials.File)
	if err != nil {
		return nil, err
	}
	topology, err := doctor.probe.Inspect(ctx, target, credentials)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("inspect MongoDB topology: %w", ctx.Err())
		}
		return nil, errors.New("inspect MongoDB topology")
	}
	output, err := doctor.runner.Run(ctx, dumpPath, "--version")
	if err != nil {
		return nil, errors.New("inspect mongodump version")
	}
	version := toolsVersionPattern.FindString(string(output))
	if version == "" || strings.TrimSpace(topology.ServerVersion) == "" {
		return nil, errors.New("MongoDB server or Database Tools version is invalid")
	}
	return &Report{Topology: *topology, ToolsVersion: version}, nil
}
