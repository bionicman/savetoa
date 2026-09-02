package redis

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
)

const serverPath = "/usr/bin/redis-server"

var serverVersionPattern = regexp.MustCompile(`\bv=([0-9]+\.[0-9]+\.[0-9]+)\b`)

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
}

func VerifyRestore(ctx context.Context, value RestoreOptions, runtime RestoreRuntime) (*localstore.Set, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if runtime == nil {
		return nil, errors.New("Redis restore runtime is required")
	}
	if value.TargetDir == "" || !filepath.IsAbs(value.TargetDir) || filepath.Clean(value.TargetDir) != value.TargetDir || value.TargetDir == string(filepath.Separator) {
		return nil, errors.New("Redis restore target must be a clean absolute path other than root")
	}
	if _, err := os.Lstat(value.TargetDir); err == nil {
		return nil, errors.New("Redis restore target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("inspect Redis restore target")
	}
	store, err := localstore.New(value.SourceRoot)
	if err != nil {
		return nil, fmt.Errorf("open Redis restore source: %w", err)
	}
	set, err := store.FindByID(value.BackupID)
	closeErr := store.Close()
	if err != nil {
		return nil, fmt.Errorf("find Redis backup: %w", err)
	}
	if closeErr != nil {
		return nil, errors.New("close Redis restore source")
	}
	if set.Manifest.CaptureDriver != "redis" || set.Manifest.Tool.Name != "redis-cli" {
		return nil, errors.New("backup is not a Redis RDB set")
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
	if err := validateMaterializedRDB(value.TargetDir); err != nil {
		return nil, err
	}
	port, err := runtime.ReservePort()
	if err != nil {
		return nil, err
	}
	server, err := runtime.Start(ctx, value.TargetDir, filepath.Join(value.TargetDir, "redis.log"), port)
	if err != nil {
		return nil, err
	}
	stopped := false
	defer func() {
		if !stopped {
			stopContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = server.Stop(stopContext)
			cancel()
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
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
			return nil, fmt.Errorf("wait for disposable Redis: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, errors.New("disposable Redis did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = server.Stop(stopContext)
	cancel()
	if err != nil {
		return nil, err
	}
	stopped = true
	succeeded = true
	return materialized, nil
}

func validateMaterializedRDB(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("inspect materialized Redis RDB")
	}
	if len(entries) != 1 || entries[0].Name() != "dump.rdb" || entries[0].Type()&os.ModeSymlink != 0 || !entries[0].Type().IsRegular() {
		return errors.New("Redis backup must materialize exactly one regular dump.rdb")
	}
	info, err := entries[0].Info()
	if err != nil || info.Size() <= 0 {
		return errors.New("materialized Redis RDB is empty")
	}
	return nil
}

type ProcessRuntime struct{}

func (ProcessRuntime) Preflight(ctx context.Context, expectedServer, expectedCLI string) error {
	serverOutput, err := exec.CommandContext(ctx, serverPath, "--version").Output()
	if err != nil {
		return errors.New("inspect disposable redis-server version")
	}
	match := serverVersionPattern.FindStringSubmatch(string(serverOutput))
	if len(match) != 2 || majorMinor(match[1]) != majorMinor(expectedServer) {
		return errors.New("disposable redis-server major/minor version does not match backup source")
	}
	cliVersion, err := (CommandVersionRunner{}).Version(ctx)
	if err != nil || cliVersion != expectedCLI {
		return errors.New("redis-cli version does not match backup tool version")
	}
	return nil
}

func (ProcessRuntime) ReservePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("reserve disposable Redis port")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, errors.New("release disposable Redis port")
	}
	return port, nil
}

type commandServer struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func (ProcessRuntime) Start(_ context.Context, dataDir, logPath string, port int) (DisposableServer, error) {
	command := exec.Command(serverPath,
		"--bind", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--protected-mode", "yes",
		"--dir", dataDir,
		"--dbfilename", "dump.rdb",
		"--appendonly", "no",
		"--save", "",
		"--daemonize", "no",
		"--logfile", logPath,
	)
	if err := command.Start(); err != nil {
		return nil, errors.New("start disposable redis-server")
	}
	server := &commandServer{command: command, done: make(chan struct{})}
	go func() {
		server.err = command.Wait()
		close(server.done)
	}()
	return server, nil
}

func (ProcessRuntime) Ready(ctx context.Context, port int) error {
	command := exec.CommandContext(ctx, cliPath,
		"--no-auth-warning", "--raw", "-h", "127.0.0.1", "-p", strconv.Itoa(port), "PING")
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != "PONG" {
		return errors.New("disposable Redis is not ready")
	}
	return nil
}

func (server *commandServer) Alive() error {
	select {
	case <-server.done:
		return errors.New("disposable redis-server exited before restore completed")
	default:
		return nil
	}
}

func (server *commandServer) Stop(ctx context.Context) error {
	if server == nil || server.command == nil || server.command.Process == nil {
		return errors.New("disposable redis-server process is unavailable")
	}
	select {
	case <-server.done:
	default:
		if err := server.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return errors.New("signal disposable redis-server")
		}
	}
	select {
	case <-server.done:
		if server.err != nil {
			return errors.New("disposable redis-server did not stop cleanly")
		}
		return nil
	case <-ctx.Done():
		return errors.New("timed out stopping disposable redis-server")
	}
}
