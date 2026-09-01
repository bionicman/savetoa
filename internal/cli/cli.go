package cli

import (
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/bionicman/savetoa/internal/buildinfo"
)

const defaultConfigPath = "/etc/savetoa/config.yml"

var plannedCommands = []string{
	"doctor",
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

	fmt.Fprintf(
		stderr,
		"savetoa: command %q is not implemented yet (config: %s)\n",
		command,
		*configPath,
	)
	return 2
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
	fmt.Fprintln(output, "  run-group GROUP     run a configured group of targets")
	fmt.Fprintln(output, "  list                list completed backup sets")
	fmt.Fprintln(output, "  verify BACKUP-ID    verify a completed backup set")
	fmt.Fprintln(output, "  restore BACKUP-ID   materialize a backup into an explicit target")
	fmt.Fprintln(output, "  prune TARGET        apply a target's retention policy")
	fmt.Fprintln(output, "  doctor TARGET       validate a target without capturing data")
	fmt.Fprintln(output, "  version             print build information")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Backup operations are not implemented in this scaffold.")
}
