package mongodb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/restore"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

const (
	mongodPath       = "/usr/bin/mongod"
	mongorestorePath = "/usr/bin/mongorestore"
)

var serverVersionPattern = regexp.MustCompile(`(?m)^db version v([0-9]+\.[0-9]+\.[0-9]+)\b`)

type RestoreOptions struct {
	SourceRoot   string
	BackupID     string
	TargetDir    string
	IdentityFile string
}

type DisposableServer interface {
	Alive() error
	Stop(context.Context) error
}

type RestoreRuntime interface {
	Preflight(context.Context, string, string) error
	ReservePort() (int, error)
	Start(context.Context, string, string, int) (DisposableServer, error)
	Ready(context.Context, int) error
	Replay(context.Context, string, int) error
}

func VerifyRestore(ctx context.Context, value RestoreOptions, runtime RestoreRuntime) (*localstore.Set, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if runtime == nil {
		return nil, errors.New("MongoDB restore runtime is required")
	}
	if value.TargetDir == "" || !filepath.IsAbs(value.TargetDir) || filepath.Clean(value.TargetDir) != value.TargetDir || value.TargetDir == string(filepath.Separator) {
		return nil, errors.New("MongoDB restore target must be a clean absolute path other than root")
	}
	if _, err := os.Lstat(value.TargetDir); err == nil {
		return nil, errors.New("MongoDB restore target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("inspect MongoDB restore target")
	}
	store, err := localstore.New(value.SourceRoot)
	if err != nil {
		return nil, fmt.Errorf("open MongoDB restore source: %w", err)
	}
	set, err := store.FindByID(value.BackupID)
	closeErr := store.Close()
	if err != nil {
		return nil, fmt.Errorf("find MongoDB backup: %w", err)
	}
	if closeErr != nil {
		return nil, errors.New("close MongoDB restore source")
	}
	if set.Manifest.CaptureDriver != "mongodb" || set.Manifest.Tool.Name != "mongodump" {
		return nil, errors.New("backup is not a MongoDB mongodump set")
	}
	if err := runtime.Preflight(ctx, set.Manifest.Source.ServerVersion, set.Manifest.Tool.Version); err != nil {
		return nil, err
	}

	created := false
	succeeded := false
	defer func() {
		if created && !succeeded {
			_ = os.RemoveAll(value.TargetDir)
		}
	}()
	materialized, err := restore.Materialize(ctx, restore.Options{
		SourceRoot: value.SourceRoot, BackupID: value.BackupID,
		TargetDir: value.TargetDir, IdentityFile: value.IdentityFile,
	})
	if err != nil {
		return nil, err
	}
	created = true
	archivePath, err := validateMaterializedMongoDB(value.TargetDir)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(value.TargetDir, "db")
	if err := os.Mkdir(dbPath, 0o700); err != nil {
		return nil, errors.New("create disposable MongoDB dbpath")
	}
	port, err := runtime.ReservePort()
	if err != nil {
		return nil, err
	}
	server, err := runtime.Start(ctx, dbPath, filepath.Join(value.TargetDir, "mongod.log"), port)
	if err != nil {
		return nil, err
	}
	stopped := false
	defer func() {
		if !stopped {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = server.Stop(stopCtx)
			cancel()
		}
	}()
	readyDeadline := time.Now().Add(30 * time.Second)
	for {
		if err := server.Alive(); err != nil {
			return nil, err
		}
		if err := runtime.Ready(ctx, port); err == nil {
			if err := server.Alive(); err != nil {
				return nil, err
			}
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for disposable MongoDB: %w", err)
		}
		if time.Now().After(readyDeadline) {
			return nil, errors.New("disposable MongoDB did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := runtime.Replay(ctx, archivePath, port); err != nil {
		return nil, err
	}
	if err := server.Alive(); err != nil {
		return nil, err
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = server.Stop(stopCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	stopped = true
	succeeded = true
	return materialized, nil
}

func validateMaterializedMongoDB(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", errors.New("inspect materialized MongoDB archive")
	}
	if len(entries) != 1 || entries[0].Name() != "dump.archive" || entries[0].Type()&os.ModeSymlink != 0 || !entries[0].Type().IsRegular() {
		return "", errors.New("MongoDB backup must materialize exactly one regular dump.archive")
	}
	return filepath.Join(root, "dump.archive"), nil
}

func (ProcessRuntime) ReservePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("reserve disposable MongoDB port")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, errors.New("release disposable MongoDB port")
	}
	return port, nil
}

type ProcessRuntime struct{}

func (ProcessRuntime) Preflight(ctx context.Context, expectedServer, expectedTools string) error {
	serverOutput, err := exec.CommandContext(ctx, mongodPath, "--version").Output()
	if err != nil {
		return errors.New("inspect disposable mongod version")
	}
	serverMatch := serverVersionPattern.FindStringSubmatch(string(serverOutput))
	if len(serverMatch) != 2 || majorMinor(serverMatch[1]) != majorMinor(expectedServer) {
		return errors.New("disposable mongod major/minor version does not match backup source")
	}
	restoreOutput, err := exec.CommandContext(ctx, mongorestorePath, "--version").Output()
	if err != nil {
		return errors.New("inspect mongorestore version")
	}
	restoreVersion := toolsVersionPattern.FindString(string(restoreOutput))
	if restoreVersion == "" || restoreVersion != expectedTools {
		return errors.New("mongorestore version does not match backup tool version")
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

type commandServer struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func (server *commandServer) Alive() error {
	select {
	case <-server.done:
		return errors.New("disposable mongod exited before restore completed")
	default:
		return nil
	}
}

func (ProcessRuntime) Start(_ context.Context, dbPath, logPath string, port int) (DisposableServer, error) {
	command := exec.Command(mongodPath,
		"--dbpath="+dbPath,
		"--bind_ip=127.0.0.1",
		"--port="+strconv.Itoa(port),
		"--nounixsocket",
		"--logpath="+logPath,
		"--logappend",
	)
	if err := command.Start(); err != nil {
		return nil, errors.New("start disposable mongod")
	}
	server := &commandServer{command: command, done: make(chan struct{})}
	go func() {
		server.err = command.Wait()
		close(server.done)
	}()
	return server, nil
}

func (server *commandServer) Stop(ctx context.Context) error {
	if server == nil || server.command == nil || server.command.Process == nil {
		return errors.New("disposable mongod process is unavailable")
	}
	select {
	case <-server.done:
	default:
		if err := server.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errors.New("signal disposable mongod")
		}
	}
	select {
	case <-server.done:
		if server.err != nil {
			return errors.New("disposable mongod exited unsuccessfully")
		}
		return nil
	case <-ctx.Done():
		_ = server.command.Process.Kill()
		return errors.New("stop disposable mongod timed out")
	}
}

func (ProcessRuntime) Ready(ctx context.Context, port int) error {
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().
		SetHosts([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}).
		SetDirect(true).
		SetServerSelectionTimeout(time.Second))
	if err != nil {
		return errors.New("connect to disposable mongod")
	}
	defer func() {
		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), time.Second)
		defer disconnectCancel()
		_ = client.Disconnect(disconnectCtx)
	}()
	return client.Ping(checkCtx, readpref.Primary())
}

func (ProcessRuntime) Replay(ctx context.Context, archivePath string, port int) error {
	command := exec.CommandContext(ctx, mongorestorePath,
		"--host=127.0.0.1",
		"--port="+strconv.Itoa(port),
		"--archive="+archivePath,
		"--oplogReplay",
		"--stopOnError",
	)
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("replay MongoDB archive: %w", ctx.Err())
		}
		return errors.New("replay MongoDB archive")
	}
	return nil
}
