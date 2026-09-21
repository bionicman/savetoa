package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bionicman/savetoa/internal/agecrypto"
	archivepkg "github.com/bionicman/savetoa/internal/archive"
	"github.com/bionicman/savetoa/internal/buildinfo"
	"github.com/bionicman/savetoa/internal/config"
	"github.com/bionicman/savetoa/internal/hook"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
	"github.com/bionicman/savetoa/internal/mariadb"
	"github.com/bionicman/savetoa/internal/mongodb"
	prunepkg "github.com/bionicman/savetoa/internal/prune"
	redisdriver "github.com/bionicman/savetoa/internal/redis"
	"github.com/bionicman/savetoa/internal/restore"
	"github.com/bionicman/savetoa/internal/s3store"
	"github.com/bionicman/savetoa/internal/spool"
	sqlite3driver "github.com/bionicman/savetoa/internal/sqlite3"
	statuspkg "github.com/bionicman/savetoa/internal/status"
	"github.com/bionicman/savetoa/internal/tardriver"
	"github.com/bionicman/savetoa/internal/targetlock"
)

const defaultConfigPath = "/etc/savetoa/config.yml"

var defaultRunPaths = runPaths{
	work:  "/var/lib/savetoa/work",
	spool: "/var/lib/savetoa/spool",
	locks: "/run/savetoa",
}

var (
	lifecycleHookRunner = hook.DefaultRunner()
	lifecycleNow        = time.Now
)

type commandLifecycle struct {
	action      string
	environment string
	target      string
	backupID    string
	repository  string
	startedAt   time.Time
}

func beginLifecycle(action, environment, target string) *commandLifecycle {
	return &commandLifecycle{action: action, environment: environment, target: target, startedAt: lifecycleNow()}
}

func (lifecycle *commandLifecycle) finish(exitCode int, stderr io.Writer) {
	if lifecycle == nil {
		return
	}
	finished := lifecycleNow()
	outcome := "failure"
	if exitCode == 0 {
		outcome = "success"
	}
	event := hook.Event{
		SchemaVersion: hook.SchemaVersion, Action: lifecycle.action, Outcome: outcome,
		Environment: lifecycle.environment, Target: lifecycle.target, BackupID: lifecycle.backupID,
		Repository: lifecycle.repository, StartedAt: lifecycle.startedAt.UTC(), FinishedAt: finished.UTC(),
		DurationMS: finished.Sub(lifecycle.startedAt).Milliseconds(), ExitCode: exitCode,
	}
	if err := lifecycleHookRunner.Run(event); err != nil {
		fmt.Fprintf(stderr, "savetoa: hook action=%s outcome=%s failed: %v\n", lifecycle.action, outcome, err)
	}
}

type runPaths struct {
	work  string
	spool string
	locks string
}

type mariaDBCapturer interface {
	Capture(context.Context, config.Target, string) (*mariadb.Capture, error)
}

type mongoDBCapturer interface {
	Capture(context.Context, config.Target, string) (*mongodb.Capture, error)
}

type redisCapturer interface {
	Capture(context.Context, config.Target, string) (*redisdriver.Capture, error)
}

type tarCapturer interface {
	Capture(context.Context, config.Target, string) (*tardriver.Capture, error)
}

type sqlite3Capturer interface {
	Capture(context.Context, config.Target, string) (*sqlite3driver.Capture, error)
}

var plannedCommands = []string{
	"deliver",
	"doctor",
	"fetch",
	"list",
	"prune",
	"restore",
	"restore-mongodb",
	"restore-redis",
	"run",
	"run-group",
	"status",
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
	if command == "restore-mongodb" {
		return runMongoDBRestore(remaining[1:], stdout, stderr)
	}
	if command == "restore-redis" {
		return runRedisRestore(remaining[1:], stdout, stderr)
	}
	if command == "deliver" {
		return runDeliver(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "fetch" {
		return runFetch(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "prune" {
		return runPrune(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "list" {
		return runList(*configPath, remaining[1:], stdout, stderr)
	}
	if command == "status" {
		return runStatus(*configPath, remaining[1:], stdout, stderr)
	}

	fmt.Fprintf(
		stderr,
		"savetoa: command %q is not implemented yet (config: %s)\n",
		command,
		*configPath,
	)
	return 2
}

type statusRepository struct {
	name   string
	driver string
	local  *localstore.Store
	s3     *s3store.Store
}

func (repository *statusRepository) Name() string   { return repository.name }
func (repository *statusRepository) Driver() string { return repository.driver }
func (repository *statusRepository) ListCompleted(ctx context.Context, environment, target string) ([]localstore.Set, error) {
	if repository.local != nil {
		return repository.local.ListCompleted(environment, target)
	}
	if repository.s3 != nil {
		return repository.s3.ListCompleted(ctx, environment, target)
	}
	return nil, errors.New("status repository is not open")
}

func runList(configPath string, args []string, stdout, stderr io.Writer) int {
	return runRepositoryReport(configPath, "list", args, false, stdout, stderr)
}

func runStatus(configPath string, args []string, stdout, stderr io.Writer) int {
	return runRepositoryReport(configPath, "status", args, true, stdout, stderr)
}

func runRepositoryReport(configPath, command string, args []string, enforce bool, stdout, stderr io.Writer) (code int) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	spoolRoot := flags.String("spool-root", defaultRunPaths.spool, "absolute path to the durable spool")
	format := flags.String("format", "text", "output format: text or json")
	var maxAge string
	if enforce {
		flags.StringVar(&maxAge, "max-age", "", "maximum acceptable age of the latest completed backup")
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "savetoa: %s requires [--spool-root ROOT] [--format text|json] TARGET\n", command)
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintln(stderr, "savetoa: format must be text or json")
		return 2
	}
	var maximumAge time.Duration
	if maxAge != "" {
		var err error
		maximumAge, err = time.ParseDuration(maxAge)
		if err != nil || maximumAge <= 0 {
			fmt.Fprintln(stderr, "savetoa: max-age must be a positive duration")
			return 2
		}
	}
	configuration, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	targetName := flags.Arg(0)
	target, ok := configuration.Targets[targetName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: target %q is not configured\n", targetName)
		return 1
	}
	var lifecycle *commandLifecycle
	if enforce {
		lifecycle = beginLifecycle("status", configuration.Environment, targetName)
		defer func() { lifecycle.finish(code, stderr) }()
	}
	repositories, closeRepositories, err := openStatusRepositories(*spoolRoot, target)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: open status repositories: %v\n", err)
		return 1
	}
	defer closeRepositories()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := statuspkg.Inspect(ctx, configuration.Environment, targetName, repositories, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %s %q failed: %v\n", command, targetName, err)
		return 1
	}
	if lifecycle != nil {
		lifecycle.backupID = report.LatestBackupID
	}
	if report.AgeSeconds != nil && *report.AgeSeconds < 0 {
		report.Status = "clock-skew"
	} else if maximumAge > 0 && report.AgeSeconds != nil && *report.AgeSeconds > int64(maximumAge/time.Second) {
		report.Status = "stale"
	}
	if *format == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintln(stderr, "savetoa: encode status report")
			return 1
		}
	} else {
		writeStatusText(stdout, report)
	}
	if enforce && report.Status != "complete" {
		return 1
	}
	return 0
}

func openStatusRepositories(spoolRoot string, target config.Target) ([]statuspkg.Repository, func(), error) {
	repositories := make([]statuspkg.Repository, 0, len(target.Destinations)+1)
	localStores := make([]*localstore.Store, 0, len(target.Destinations)+1)
	closeRepositories := func() {
		for _, store := range localStores {
			_ = store.Close()
		}
	}
	identities := make(map[string]string, len(target.Destinations)+1)
	openLocal := func(name, root string) error {
		identity := "local:" + root
		if existing, duplicate := identities[identity]; duplicate {
			return fmt.Errorf("repositories %q and %q use the same local root", existing, name)
		}
		store, err := localstore.New(root)
		if err != nil {
			return fmt.Errorf("open repository %q: %w", name, err)
		}
		identities[identity] = name
		localStores = append(localStores, store)
		repositories = append(repositories, &statusRepository{name: name, driver: "local", local: store})
		return nil
	}
	if err := openLocal("spool", spoolRoot); err != nil {
		return nil, closeRepositories, err
	}
	destinationNames := make([]string, 0, len(target.Destinations))
	for name := range target.Destinations {
		destinationNames = append(destinationNames, name)
	}
	slices.Sort(destinationNames)
	for _, name := range destinationNames {
		destination := target.Destinations[name]
		switch destination.Driver {
		case "local":
			if err := openLocal(name, destination.Path); err != nil {
				closeRepositories()
				return nil, func() {}, err
			}
		case "s3":
			identity := strings.Join([]string{"s3", destination.Endpoint, destination.Region, destination.Bucket, destination.Prefix}, "\x00")
			if existing, duplicate := identities[identity]; duplicate {
				closeRepositories()
				return nil, func() {}, fmt.Errorf("repositories %q and %q use the same S3 prefix", existing, name)
			}
			store, err := openS3Destination(destination)
			if err != nil {
				closeRepositories()
				return nil, func() {}, fmt.Errorf("open repository %q: %w", name, err)
			}
			identities[identity] = name
			repositories = append(repositories, &statusRepository{name: name, driver: "s3", s3: store})
		default:
			closeRepositories()
			return nil, func() {}, fmt.Errorf("repository %q uses unsupported driver %q", name, destination.Driver)
		}
	}
	return repositories, closeRepositories, nil
}

func writeStatusText(output io.Writer, report *statuspkg.Report) {
	age := "none"
	if report.AgeSeconds != nil {
		age = strconv.FormatInt(*report.AgeSeconds, 10) + "s"
	}
	fmt.Fprintf(output, "target=%s status=%s latest=%s age=%s repositories=%d backups=%d\n",
		report.Target, report.Status, report.LatestBackupID, age, len(report.Repositories), len(report.Backups))
	for _, backup := range report.Backups {
		fmt.Fprintf(output, "target=%s backup_id=%s completed=%s size=%d repositories=%s path=%s\n",
			report.Target, backup.BackupID, backup.CompletedAt.UTC().Format(time.RFC3339Nano), backup.SizeBytes,
			strings.Join(backup.Repositories, ","), backup.RelativePath)
	}
}

func runPrune(configPath string, args []string, stdout, stderr io.Writer) (code int) {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	spoolRoot := flags.String("spool-root", defaultRunPaths.spool, "absolute path to the durable spool")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "savetoa: prune requires [--spool-root ROOT] TARGET")
		return 2
	}
	configuration, err := readConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: %v\n", err)
		return 1
	}
	targetName := flags.Arg(0)
	target, ok := configuration.Targets[targetName]
	if !ok {
		fmt.Fprintf(stderr, "savetoa: target %q is not configured\n", targetName)
		return 1
	}
	lifecycle := beginLifecycle("prune", configuration.Environment, targetName)
	defer func() { lifecycle.finish(code, stderr) }()
	if target.Retention == nil {
		fmt.Fprintf(stdout, "target=%s status=skipped reason=no-retention-policy\n", targetName)
		return 0
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

	var localStores []*localstore.Store
	defer func() {
		for _, store := range localStores {
			_ = store.Close()
		}
	}()
	repositories := make([]prunepkg.Repository, 0, len(target.Destinations)+1)
	identities := make(map[string]string, len(target.Destinations)+1)
	openLocal := func(name, root string) bool {
		identity := "local:" + root
		if existing, duplicate := identities[identity]; duplicate {
			fmt.Fprintf(stderr, "savetoa: retention repositories %q and %q use the same local root\n", existing, name)
			return false
		}
		store, openErr := localstore.New(root)
		if openErr != nil {
			fmt.Fprintf(stderr, "savetoa: open retention repository %q: %v\n", name, openErr)
			return false
		}
		identities[identity] = name
		localStores = append(localStores, store)
		repositories = append(repositories, &prunepkg.LocalRepository{RepositoryName: name, Store: store})
		return true
	}
	if !openLocal("spool", *spoolRoot) {
		return 1
	}
	destinationNames := make([]string, 0, len(target.Destinations))
	for name := range target.Destinations {
		destinationNames = append(destinationNames, name)
	}
	slices.Sort(destinationNames)
	for _, name := range destinationNames {
		destination := target.Destinations[name]
		switch destination.Driver {
		case "local":
			if !openLocal(name, destination.Path) {
				return 1
			}
		case "s3":
			identity := strings.Join([]string{"s3", destination.Endpoint, destination.Region, destination.Bucket, destination.Prefix}, "\x00")
			if existing, duplicate := identities[identity]; duplicate {
				fmt.Fprintf(stderr, "savetoa: retention repositories %q and %q use the same S3 prefix\n", existing, name)
				return 1
			}
			store, openErr := openS3MaintenanceDestination(destination)
			if openErr != nil {
				fmt.Fprintf(stderr, "savetoa: open retention repository %q: %v\n", name, openErr)
				return 1
			}
			identities[identity] = name
			repositories = append(repositories, &prunepkg.S3Repository{RepositoryName: name, Store: store})
		}
	}
	results, err := prunepkg.Execute(ctx, configuration.Environment, targetName, *target.Retention, repositories)
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: prune %q failed: %v\n", targetName, err)
		return 1
	}
	for _, result := range results {
		fmt.Fprintf(stdout, "target=%s repository=%s status=complete kept=%d pruned=%d\n",
			targetName, result.Repository, result.Kept, result.Pruned)
	}
	return 0
}

func runRedisRestore(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore-redis", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceRoot := flags.String("source-root", "", "absolute path to a local backup store")
	targetDir := flags.String("target-dir", "", "absolute path to a new disposable restore directory")
	identityFile := flags.String("identity-file", "", "0600 age X25519 identity file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *sourceRoot == "" || *targetDir == "" {
		fmt.Fprintln(stderr, "savetoa: restore-redis requires --source-root ROOT --target-dir DIR BACKUP-ID")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	set, err := redisdriver.VerifyRestore(ctx, redisdriver.RestoreOptions{
		SourceRoot: *sourceRoot, BackupID: flags.Arg(0), TargetDir: *targetDir, IdentityFile: *identityFile,
	}, redisdriver.ProcessRuntime{})
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: Redis restore verification failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backup_id=%s target=%s driver=redis status=verified path=%s\n",
		set.Manifest.BackupID, set.Manifest.Target, *targetDir)
	return 0
}

func runMongoDBRestore(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore-mongodb", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sourceRoot := flags.String("source-root", "", "absolute path to a local backup store")
	targetDir := flags.String("target-dir", "", "absolute path to a new disposable restore directory")
	identityFile := flags.String("identity-file", "", "0600 age X25519 identity file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *sourceRoot == "" || *targetDir == "" {
		fmt.Fprintln(stderr, "savetoa: restore-mongodb requires --source-root ROOT --target-dir DIR BACKUP-ID")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	set, err := mongodb.VerifyRestore(ctx, mongodb.RestoreOptions{
		SourceRoot: *sourceRoot, BackupID: flags.Arg(0), TargetDir: *targetDir, IdentityFile: *identityFile,
	}, mongodb.ProcessRuntime{})
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: MongoDB restore verification failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backup_id=%s target=%s driver=mongodb status=verified path=%s\n",
		set.Manifest.BackupID, set.Manifest.Target, *targetDir)
	return 0
}

func runFetch(configPath string, args []string, stdout, stderr io.Writer) (code int) {
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
	lifecycle := beginLifecycle("fetch", configuration.Environment, targetName)
	lifecycle.backupID = backupID
	lifecycle.repository = sourceName
	defer func() { lifecycle.finish(code, stderr) }()
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

func runDeliver(configPath string, args []string, stdout, stderr io.Writer) (code int) {
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
	lifecycle := beginLifecycle("deliver", configuration.Environment, targetName)
	lifecycle.backupID = backupID
	lifecycle.repository = destinationName
	defer func() { lifecycle.finish(code, stderr) }()
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

func openS3MaintenanceDestination(destination config.Destination) (*s3store.Store, error) {
	if destination.MaintenanceCredentials == nil {
		return nil, errors.New("S3 maintenance credentials file is required")
	}
	credentials, err := s3store.ReadCredentialsFile(destination.MaintenanceCredentials.File)
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

func runBackup(configPath string, args []string, stdout, stderr io.Writer) (code int) {
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
	lifecycle := beginLifecycle("run", configuration.Environment, targetName)
	defer func() { lifecycle.finish(code, stderr) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var set *localstore.Set
	switch target.Driver {
	case "mariadb":
		set, err = executeMariaDBRun(ctx, configuration.Environment, targetName, target, defaultRunPaths, mariadb.NewCapturer())
	case "mongodb":
		set, err = executeMongoDBRun(ctx, configuration.Environment, targetName, target, defaultRunPaths, mongodb.NewCapturer())
	case "redis":
		set, err = executeRedisRun(ctx, configuration.Environment, targetName, target, defaultRunPaths, redisdriver.NewCapturer())
	case "tar":
		set, err = executeTarRun(ctx, configuration.Environment, targetName, target, defaultRunPaths, tardriver.NewCapturer())
	case "sqlite3":
		set, err = executeSQLite3Run(ctx, configuration.Environment, targetName, target, defaultRunPaths, sqlite3driver.NewCapturer())
	default:
		fmt.Fprintf(stderr, "savetoa: run does not support driver %q\n", target.Driver)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "savetoa: run %q failed: %v\n", targetName, err)
		return 1
	}
	lifecycle.backupID = set.Manifest.BackupID
	fmt.Fprintf(stdout, "target=%s driver=%s status=complete backup_id=%s path=%s\n",
		targetName, target.Driver, set.Manifest.BackupID, set.RelativePath)
	return 0
}

func executeSQLite3Run(
	ctx context.Context,
	environment string,
	targetName string,
	target config.Target,
	paths runPaths,
	capturer sqlite3Capturer,
) (*localstore.Set, error) {
	if ctx == nil || capturer == nil {
		return nil, errors.New("context and SQLite capturer are required")
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
		return nil, errors.New("at least one destination is required")
	}
	slices.SortFunc(destinations, func(left, right destinationHandle) int {
		return strings.Compare(left.name, right.name)
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
		FormatVersion: manifest.FormatVersion, BackupID: backupID, Target: targetName,
		CaptureDriver: "sqlite3", StartedAt: started, CompletedAt: time.Now().UTC(),
		Tool:     manifest.Tool{Name: "sqlite3", Version: capture.Report.Version},
		Source:   manifest.Source{ServerVersion: capture.Report.Version, Replication: map[string]string{}},
		Artifact: manifest.Artifact{Filename: filename}, Transformations: transformations,
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

func executeTarRun(
	ctx context.Context,
	environment string,
	targetName string,
	target config.Target,
	paths runPaths,
	capturer tarCapturer,
) (*localstore.Set, error) {
	if ctx == nil || capturer == nil {
		return nil, errors.New("context and tar capturer are required")
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
		return nil, errors.New("at least one destination is required")
	}
	slices.SortFunc(destinations, func(left, right destinationHandle) int {
		return strings.Compare(left.name, right.name)
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
	var payload io.Reader = capture.Reader()
	var compressed io.ReadCloser
	if target.Compression != nil {
		filename += ".zst"
		level := target.Compression.Level
		transformations = append(transformations, manifest.Transformation{Driver: "zstd", Level: &level})
		compressed, err = archivepkg.ZstdReader(ctx, payload, level)
		if err != nil {
			return nil, err
		}
		defer compressed.Close()
		payload = compressed
	}
	value := manifest.Manifest{
		FormatVersion: manifest.FormatVersion, BackupID: backupID, Target: targetName,
		CaptureDriver: "tar", StartedAt: started, CompletedAt: time.Now().UTC(),
		Tool:     manifest.Tool{Name: "tar", Version: capture.Report.Version},
		Source:   manifest.Source{Replication: map[string]string{}},
		Artifact: manifest.Artifact{Filename: filename}, Transformations: transformations,
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

func executeRedisRun(
	ctx context.Context,
	environment string,
	targetName string,
	target config.Target,
	paths runPaths,
	capturer redisCapturer,
) (*localstore.Set, error) {
	if ctx == nil || capturer == nil {
		return nil, errors.New("context and Redis capturer are required")
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
		return nil, errors.New("at least one destination is required")
	}
	slices.SortFunc(destinations, func(left, right destinationHandle) int {
		return strings.Compare(left.name, right.name)
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
		FormatVersion: manifest.FormatVersion, BackupID: backupID, Target: targetName,
		CaptureDriver: "redis", StartedAt: started, CompletedAt: time.Now().UTC(),
		Tool: manifest.Tool{Name: "redis-cli", Version: capture.Report.CLIversion},
		Source: manifest.Source{ServerVersion: capture.Report.ServerVersion, Replication: map[string]string{
			"source_host":        capture.Report.MasterHost,
			"source_port":        strconv.Itoa(capture.Report.MasterPort),
			"role":               capture.Report.Role,
			"replication_id":     capture.Report.ReplicationID,
			"replication_offset": strconv.FormatInt(capture.Report.ReplicationOffset, 10),
			"lag_seconds":        strconv.FormatInt(int64(capture.Report.Lag/time.Second), 10),
			"lastsave_unix":      strconv.FormatInt(capture.Report.LastSave, 10),
			"read_only":          strconv.FormatBool(capture.Report.ReadOnly),
			"priority":           strconv.Itoa(capture.Report.Priority),
			"aof_enabled":        strconv.FormatBool(capture.Report.AOFEnabled),
		}},
		Artifact: manifest.Artifact{Filename: filename}, Transformations: transformations,
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

func executeMongoDBRun(
	ctx context.Context,
	environment string,
	targetName string,
	target config.Target,
	paths runPaths,
	capturer mongoDBCapturer,
) (*localstore.Set, error) {
	if ctx == nil || capturer == nil {
		return nil, errors.New("context and MongoDB capturer are required")
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
		return nil, errors.New("at least one destination is required")
	}
	slices.SortFunc(destinations, func(left, right destinationHandle) int {
		return strings.Compare(left.name, right.name)
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
		FormatVersion: manifest.FormatVersion, BackupID: backupID, Target: targetName,
		CaptureDriver: "mongodb", StartedAt: started, CompletedAt: time.Now().UTC(),
		Tool: manifest.Tool{Name: "mongodump", Version: capture.Report.ToolsVersion},
		Source: manifest.Source{ServerVersion: capture.Report.ServerVersion, Replication: map[string]string{
			"set_name":       capture.Report.SetName,
			"member":         capture.Report.Member,
			"state":          capture.Report.State,
			"optime":         capture.Report.Optime.Format(time.RFC3339Nano),
			"lag_seconds":    strconv.FormatInt(int64(capture.Report.Lag/time.Second), 10),
			"config_version": strconv.Itoa(capture.Report.ConfigVersion),
		}},
		Artifact: manifest.Artifact{Filename: filename}, Transformations: transformations,
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

func runDoctor(configPath string, args []string, stdout, stderr io.Writer) (code int) {
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
	lifecycle := beginLifecycle("doctor", configuration.Environment, targetName)
	defer func() { lifecycle.finish(code, stderr) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch target.Driver {
	case "mariadb":
		report, err := mariadb.NewDoctor().Check(ctx, target)
		if err != nil {
			fmt.Fprintf(stderr, "savetoa: doctor %q failed: %v\n", targetName, err)
			return 1
		}
		fmt.Fprintf(stdout, "target=%s driver=mariadb status=healthy server=%s backup=%s source=%s:%d lag=%s gtid=%s\n",
			targetName, report.ServerVersion, report.BackupVersion, report.SourceHost,
			report.SourcePort, report.Lag, report.GTIDPosition)
	case "mongodb":
		report, err := mongodb.NewDoctor().Check(ctx, target)
		if err != nil {
			fmt.Fprintf(stderr, "savetoa: doctor %q failed: %v\n", targetName, err)
			return 1
		}
		fmt.Fprintf(stdout, "target=%s driver=mongodb status=healthy server=%s tools=%s set=%s member=%s lag=%s\n",
			targetName, report.ServerVersion, report.ToolsVersion, report.SetName, report.Member, report.Lag)
	case "redis":
		report, err := redisdriver.NewDoctor().Check(ctx, target)
		if err != nil {
			fmt.Fprintf(stderr, "savetoa: doctor %q failed: %v\n", targetName, err)
			return 1
		}
		fmt.Fprintf(stdout, "target=%s driver=redis status=healthy server=%s cli=%s source=%s:%d lag=%s offset=%d lastsave=%d\n",
			targetName, report.ServerVersion, report.CLIversion, report.MasterHost,
			report.MasterPort, report.Lag, report.ReplicationOffset, report.LastSave)
	case "tar":
		report, err := tardriver.NewCapturer().Check(ctx, target)
		if err != nil {
			fmt.Fprintf(stderr, "savetoa: doctor %q failed: %v\n", targetName, err)
			return 1
		}
		fmt.Fprintf(stdout, "target=%s driver=tar status=healthy tool=%s paths=%d\n",
			targetName, report.Version, report.Paths)
	case "sqlite3":
		report, err := sqlite3driver.NewCapturer().Check(ctx, target)
		if err != nil {
			fmt.Fprintf(stderr, "savetoa: doctor %q failed: %v\n", targetName, err)
			return 1
		}
		fmt.Fprintf(stdout, "target=%s driver=sqlite3 status=healthy tool=%s\n",
			targetName, report.Version)
	default:
		fmt.Fprintf(stderr, "savetoa: doctor does not support driver %q\n", target.Driver)
		return 1
	}
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
	fmt.Fprintln(output, "  list [OPTIONS] TARGET  list completed sets and repository presence")
	fmt.Fprintln(output, "  status [OPTIONS] TARGET  check latest-set completeness and freshness")
	fmt.Fprintln(output, "  verify BACKUP-ID    verify a completed backup set")
	fmt.Fprintln(output, "  restore [OPTIONS] BACKUP-ID  verify and materialize into a new directory")
	fmt.Fprintln(output, "  restore-mongodb [OPTIONS] BACKUP-ID  replay into a disposable local mongod")
	fmt.Fprintln(output, "  restore-redis [OPTIONS] BACKUP-ID  load RDB into a disposable local redis-server")
	fmt.Fprintln(output, "  prune [--spool-root ROOT] TARGET  apply retention to completed sets")
	fmt.Fprintln(output, "  doctor TARGET       validate a target without capturing data")
	fmt.Fprintln(output, "  version             print build information")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "MariaDB, MongoDB, Redis, SQLite, and tar run, S3 delivery, status, retention, restore, and doctor are implemented; other operations fail closed.")
}
