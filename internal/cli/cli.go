package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/bionicman/savetoa/internal/agecrypto"
	archivepkg "github.com/bionicman/savetoa/internal/archive"
	"github.com/bionicman/savetoa/internal/buildinfo"
	"github.com/bionicman/savetoa/internal/config"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
	"github.com/bionicman/savetoa/internal/mariadb"
	"github.com/bionicman/savetoa/internal/restore"
	"github.com/bionicman/savetoa/internal/s3store"
	"github.com/bionicman/savetoa/internal/spool"
	"github.com/bionicman/savetoa/internal/targetlock"
)

const defaultConfigPath = "/etc/savetoa/config.yml"

var defaultRunPaths = runPaths{
	work:  "/var/lib/savetoa/work",
	spool: "/var/lib/savetoa/spool",
	locks: "/run/savetoa",
}

type runPaths struct {
	work  string
	spool string
	locks string
}

type mariaDBCapturer interface {
	Capture(context.Context, config.Target, string) (*mariadb.Capture, error)
}

var plannedCommands = []string{
	"deliver",
	"doctor",
	"fetch",
	"list",
	"prune",
	"restore",
	"run",
	"run-group",
	"verify",
}

func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("savetoa", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "path to the configuration file")
	showVersion := flags.Bool("version", false, "print version information")
	flags.Usage = func() { writeUsage(stderr) }

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		writeVersion(stdout)
		return 0
	}

	remaining := flags.Args()
	if len(remaining) == 0 {
		writeUsage(stdout)
		return 0
	}

	command := remaining[0]
	if command == "help" {
		writeUsage(stdout)
		return 0
	}
	if command == "version" {
		writeVersion(stdout)
		return 0
	}
	if !slices.Contains(plannedCommands, command) {
		fmt.Fprintf(stderr, "savetoa: unknown command %q\n", command)
		writeUsage(stderr)
		return 2
	}
	if command == "doctor" {
		return runDoctor(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "run" {
		return runBackup(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "restore" {
		return runRestore(remaining[1:], stdout, stderr)
	}
	if command == "deliver" {
		return runDeliver(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "fetch" {
		return runFetch(*configPath, remaining[1:], stdout, stderr)
	}

	fmt.Fprintf(
		stderr,
		"savetoa: command %q is not implemented yet (config: %s)\n",
		command,
		*configPath,
	)
	return 2
}

func runFetch(configPath string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fetch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	spoolRoot := flags.String("spool-root", defaultRunPaths.spool, "absolute path to the destination spool")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 3 {
		fmt.Fprintln(stderr, "savetoa: fetch requires [--spool-root ROOT] TARGET S3-SOURCE BACKUP-ID")
		return 2
	}
	configuration, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	targetName, sourceName, backupID := flags.Arg(0), flags.Arg(1), flags.Arg(2)
	target, ok := configuration.Targets[targetName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: target %q is not configured\n", targetName)
		return 1
	}
	sourceConfig, ok := target.Destinations[sourceName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: destination %q is not configured for target %q\n", sourceName, targetName)
		return 1
	}
	if sourceConfig.Driver != "s3" {
		fmt.Fprintf(stderr, "savetoa: destination %q is not an S3 source\n", sourceName)
		return 1
	}
	relativePath, err := backupSetRelativePath(configuration.Environment, targetName, backupID)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: invalid backup ID for fetch: %v\n", err)
		return 1
	}
	source, err := openS3Destination(sourceConfig)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open S3 source %q: %v\n", sourceName, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	locks, err := targetlock.New(defaultRunPaths.locks)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open target locks: %v\n", err)
		return 1
	}
	defer locks.Close()
	lock, err := locks.Acquire(ctx, targetName)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: lock target %q: %v\n", targetName, err)
		return 1
	}
	defer lock.Release()
	staging, err := spool.New(*spoolRoot)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open spool: %v\n", err)
		return 1
	}
	defer staging.Close()
	set, err := staging.FetchS3(ctx, relativePath, source)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: fetch failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "target=%s source=%s driver=s3 status=complete backup_id=%s path=%s\n",
		targetName, sourceName, backupID, set.RelativePath)
	return 0
}

func backupSetRelativePath(environment, target, backupID string) (string, error) {
	if len(backupID) < len("20060102") {
		return "", errors.New("backup ID does not contain a date")
	}
	date, err := time.Parse("20060102", backupID[:8])
	if err != nil {
		return "", errors.New("backup ID does not start with a valid UTC date")
	}
	relativePath := filepath.Join(environment, target, date.Format("2006"), date.Format("01"), date.Format("02"), backupID)
	if err := localstore.ValidateSetPath(relativePath); err != nil {
		return "", err
	}
	return relativePath, nil
}

func runDeliver(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "savetoa: deliver requires TARGET DESTINATION BACKUP-ID")
		return 2
	}
	configuration, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	targetName, destinationName, backupID := args[0], args[1], args[2]
	target, ok := configuration.Targets[targetName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: target %q is not configured\n", targetName)
		return 1
	}
	destinationConfig, ok := target.Destinations[destinationName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: destination %q is not configured for target %q\n", destinationName, targetName)
		return 1
	}
	if destinationConfig.Driver != "s3" {
		fmt.Fprintf(stderr, "savetoa: destination %q is not an S3 destination\n", destinationName)
		return 1
	}
	destination, err := openS3Destination(destinationConfig)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open destination %q: %v\n", destinationName, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	locks, err := targetlock.New(defaultRunPaths.locks)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open target locks: %v\n", err)
		return 1
	}
	defer locks.Close()
	lock, err := locks.Acquire(ctx, targetName)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: lock target %q: %v\n", targetName, err)
		return 1
	}
	defer lock.Release()
	staging, err := spool.New(defaultRunPaths.spool)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open spool: %v\n", err)
		return 1
	}
	defer staging.Close()
	set, err := staging.FindByID(backupID)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	if set.Manifest.Target != targetName || set.Environment != configuration.Environment {
		fmt.Fprintln(stderr, "savetoa: staged backup identity does not match target and environment")
		return 1
	}
	if _, err := staging.DeliverS3(ctx, set.RelativePath, destination); err != nil {
		fmt.Fprintf(stderr, "savetoa: deliver failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "target=%s destination=%s driver=s3 status=complete backup_id=%s key=%s\n",
		targetName, destinationName, backupID, destination.SetPrefix(set.RelativePath))
	return 0
}

func openS3Destination(destination config.Destination) (*s3store.Store, error) {
	if destination.Credentials == nil {
		return nil, errors.New("S3 credentials file is required")
	}
	credentials, err := s3store.ReadCredentialsFile(destination.Credentials.File)
	if err != nil {
		return nil, err
	}
	return s3store.New(s3store.Options{
		Endpoint: destination.Endpoint, Region: destination.Region, Bucket: destination.Bucket,
		Prefix: destination.Prefix, Credentials: credentials,
	})
}

func runRestore(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceRoot := flags.String("source-root", "", "absolute path to a local backup store")
	targetDir := flags.String("target-dir", "", "absolute path to a new restore directory")
	identityFile := flags.String("identity-file", "", "0600 age X25519 identity file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *sourceRoot == "" || *targetDir == "" {
		fmt.Fprintln(stderr, "savetoa: restore requires --source-root ROOT --target-dir DIR BACKUP-ID")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	set, err := restore.Materialize(ctx, restore.Options{
		SourceRoot: *sourceRoot, BackupID: flags.Arg(0), TargetDir: *targetDir, IdentityFile: *identityFile,
	})
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: restore failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backup_id=%s target=%s driver=%s status=materialized path=%s\n",
		set.Manifest.BackupID, set.Manifest.Target, set.Manifest.CaptureDriver, *targetDir)
	return 0
}

func runBackup(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "savetoa: run requires exactly one TARGET")
		return 2
	}
	configuration, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	targetName := args[0]
	target, ok := configuration.Targets[targetName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: target %q is not configured\n", targetName)
		return 1
	}
	if target.Driver != "mariadb" {
		fmt.Fprintf(stderr, "savetoa: run does not support driver %q\n", target.Driver)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	set, err := executeMariaDBRun(ctx, configuration.Environment, targetName, target, defaultRunPaths, mariadb.NewCapturer())
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: run %q failed: %v\n", targetName, err)
		return 1
	}
	fmt.Fprintf(stdout, "target=%s driver=mariadb status=complete backup_id=%s path=%s\n",
		targetName, set.Manifest.BackupID, set.RelativePath)
	return 0
}

func executeMariaDBRun(
	ctx context.Context,
	environment string,
	targetName string,
	target config.Target,
	paths runPaths,
	capturer mariaDBCapturer,
) (*localstore.Set, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if capturer == nil {
		return nil, fmt.Errorf("MariaDB capturer is required")
	}

	type destinationHandle struct {
		name   string
		driver string
		local  *localstore.Store
		s3     *s3store.Store
	}
	destinations := make([]destinationHandle, 0, len(target.Destinations))
	for name, destination := range target.Destinations {
		handle := destinationHandle{name: name, driver: destination.Driver}
		switch destination.Driver {
		case "local":
			store, err := localstore.New(destination.Path)
			if err != nil {
				return nil, fmt.Errorf("open destination %q: %w", name, err)
			}
			handle.local = store
		case "s3":
			store, err := openS3Destination(destination)
			if err != nil {
				return nil, fmt.Errorf("open destination %q: %w", name, err)
			}
			handle.s3 = store
		default:
			return nil, fmt.Errorf("destination %q: driver %q is not implemented", name, destination.Driver)
		}
		destinations = append(destinations, handle)
	}
	defer func() {
		for _, destination := range destinations {
			if destination.local != nil {
				_ = destination.local.Close()
			}
		}
	}()
	if len(destinations) == 0 {
		return nil, fmt.Errorf("at least one destination is required")
	}
	slices.SortFunc(destinations, func(left, right destinationHandle) int {
		if left.name < right.name {
			return -1
		}
		if left.name > right.name {
			return 1
		}
		return 0
	})

	var encryptor *agecrypto.Encryptor
	if target.Encryption != nil {
		file, err := openRegularFile(target.Encryption.RecipientsFile)
		if err != nil {
			return nil, fmt.Errorf("open age recipients: %w", err)
		}
		encryptor, err = agecrypto.ParseRecipients(file)
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close age recipients: %w", closeErr)
		}
	}

	locks, err := targetlock.New(paths.locks)
	if err != nil {
		return nil, err
	}
	defer locks.Close()
	lock, err := locks.Acquire(ctx, targetName)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	started := time.Now().UTC()
	capture, err := capturer.Capture(ctx, target, paths.work)
	if err != nil {
		return nil, err
	}
	defer capture.Close()

	backupID, err := newBackupID(started)
	if err != nil {
		return nil, err
	}
	filename := "payload.tar"
	transformations := make([]manifest.Transformation, 0, 2)
	var payload io.ReadCloser
	if target.Compression == nil {
		payload, err = archivepkg.TarReader(ctx, capture.Path())
	} else {
		filename += ".zst"
		level := target.Compression.Level
		transformations = append(transformations, manifest.Transformation{Driver: "zstd", Level: &level})
		payload, err = archivepkg.TarZstdReader(ctx, capture.Path(), level)
	}
	if err != nil {
		return nil, err
	}
	defer payload.Close()

	value := manifest.Manifest{
		FormatVersion: manifest.FormatVersion,
		BackupID:      backupID,
		Target:        targetName,
		CaptureDriver: "mariadb",
		StartedAt:     started,
		CompletedAt:   time.Now().UTC(),
		Tool:          manifest.Tool{Name: "mariadb-backup", Version: capture.Report.BackupVersion},
		Source: manifest.Source{
			ServerVersion: capture.Report.ServerVersion,
			Replication: map[string]string{
				"source_host": capture.Report.SourceHost,
				"source_port": strconv.Itoa(capture.Report.SourcePort),
				"source_user": capture.Report.SourceUser,
				"gtid":        capture.GTIDPosition,
				"lag_seconds": strconv.FormatInt(int64(capture.Report.Lag/time.Second), 10),
			},
		},
		Artifact:        manifest.Artifact{Filename: filename},
		Transformations: transformations,
	}

	staging, err := spool.New(paths.spool)
	if err != nil {
		return nil, err
	}
	defer staging.Close()
	var staged *localstore.Set
	if encryptor == nil {
		staged, err = staging.Stage(ctx, environment, value, payload)
	} else {
		staged, err = staging.StageEncrypted(ctx, environment, value, payload, encryptor)
	}
	if err != nil {
		return nil, err
	}
	if _, err := staging.Verify(ctx, staged.RelativePath); err != nil {
		return nil, err
	}

	delivered := staged
	for _, destination := range destinations {
		switch destination.driver {
		case "local":
			delivered, err = staging.DeliverLocal(ctx, staged.RelativePath, destination.local)
		case "s3":
			delivered, err = staging.DeliverS3(ctx, staged.RelativePath, destination.s3)
		}
		if err != nil {
			return nil, fmt.Errorf("deliver destination %q: %w; staged backup remains at %s", destination.name, err, staged.RelativePath)
		}
	}
	if err := capture.Close(); err != nil {
		return nil, err
	}
	return delivered, nil
}

func newBackupID(started time.Time) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate backup ID: %w", err)
	}
	return started.UTC().Format("20060102t150405z") + "-" + hex.EncodeToString(random), nil
}

func openRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path must be a regular file, not a symlink")
	}
	return os.Open(path)
}

func readConfig(configPath string) (*config.Config, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	configuration, err := config.Read(file)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return configuration, nil
}

func runDoctor(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "savetoa: doctor requires exactly one TARGET")
		return 2
	}
	configuration, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	targetName := args[0]
	target, ok := configuration.Targets[targetName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: target %q is not configured\n", targetName)
		return 1
	}
	if target.Driver != "mariadb" {
		fmt.Fprintf(stderr, "savetoa: doctor does not support driver %q\n", target.Driver)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := mariadb.NewDoctor().Check(ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: doctor %q failed: %v\n", targetName, err)
		return 1
	}
	fmt.Fprintf(stdout, "target=%s driver=mariadb status=healthy server=%s backup=%s source=%s:%d lag=%s gtid=%s\n",
		targetName, report.ServerVersion, report.BackupVersion, report.SourceHost,
		report.SourcePort, report.Lag, report.GTIDPosition)
	return 0
}

func writeVersion(output io.Writer) {
	fmt.Fprintf(
		output,
		"savetoa %s (commit %s, built %s)\n",
		buildinfo.Version,
		buildinfo.Commit,
		buildinfo.Date,
	)
}

func writeUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: savetoa [--config PATH] <command> [arguments]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Commands:")
	fmt.Fprintln(output, "  run TARGET          capture and deliver one configured target")
	fmt.Fprintln(output, "  deliver TARGET DESTINATION BACKUP-ID  retry S3 delivery from the spool")
	fmt.Fprintln(output, "  fetch [--spool-root ROOT] TARGET S3-SOURCE BACKUP-ID  import an S3 set")
	fmt.Fprintln(output, "  run-group GROUP     run a configured group of targets")
	fmt.Fprintln(output, "  list                list completed backup sets")
	fmt.Fprintln(output, "  verify BACKUP-ID    verify a completed backup set")
	fmt.Fprintln(output, "  restore [OPTIONS] BACKUP-ID  verify and materialize into a new directory")
	fmt.Fprintln(output, "  prune TARGET        apply a target's retention policy")
	fmt.Fprintln(output, "  doctor TARGET       validate a target without capturing data")
	fmt.Fprintln(output, "  version             print build information")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "MariaDB run, S3 delivery, restore, and doctor are implemented; other operations fail closed.")
}
