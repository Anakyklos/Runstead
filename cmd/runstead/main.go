package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/trace"
)

const (
	exitSuccess     = 0
	exitUsage       = 2
	exitUnavailable = 3
	exitInterrupted = 130
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || isHelp(args[0]) {
		printRootHelp(out)
		return exitSuccess
	}

	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], out, errOut)
	case "inspect":
		return placeholderCommand(ctx, "inspect", args[1:], out, errOut)
	case "resume":
		return placeholderCommand(ctx, "resume", args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "runstead: unknown command %q\n\n", args[0])
		printRootHelp(errOut)
		return exitUsage
	}
}

func runCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printRunHelp(out)
		return exitSuccess
	}

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	workspace := ""
	logLevel := ""
	flags.StringVar(&workspace, "workspace", "", "workspace path (default: RUNSTEAD_WORKSPACE or .)")
	flags.StringVar(&logLevel, "log-level", "", "log level: debug, info, warn or error")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(errOut, "run: invalid flags: %v\n", err)
		printRunHelp(errOut)
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(errOut, "run: unexpected argument %q\n", flags.Arg(0))
		printRunHelp(errOut)
		return exitUsage
	}

	cfg, err := config.Resolve(config.Overrides{
		Workspace:    workspace,
		WorkspaceSet: flagWasSet(flags, "workspace"),
		LogLevel:     logLevel,
		LogLevelSet:  flagWasSet(flags, "log-level"),
	}, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(errOut, "run: invalid configuration: %v\n", err)
		return exitUsage
	}
	level, err := trace.ParseLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(errOut, "run: invalid configuration: %v\n", err)
		return exitUsage
	}
	logger := trace.NewLogger(errOut, level)
	if err := ctx.Err(); err != nil {
		logger.WarnContext(ctx, "run canceled", "error", err.Error())
		fmt.Fprintln(errOut, "run: canceled")
		return exitInterrupted
	}

	logger.InfoContext(ctx, "run command unavailable", "command", "run", "workspace", cfg.Workspace)
	fmt.Fprintln(errOut, "run: agent loop is not implemented in issue #3; provider integration is deferred to issue #4 and the full loop to issue #7")
	return exitUnavailable
}

func placeholderCommand(ctx context.Context, name string, args []string, out, errOut io.Writer) int {
	if hasHelp(args) {
		printPlaceholderHelp(out, name)
		return exitSuccess
	}

	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(errOut, "%s: invalid flags: %v\n", name, err)
		printPlaceholderHelp(errOut, name)
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(errOut, "%s: unexpected argument %q\n", name, flags.Arg(0))
		printPlaceholderHelp(errOut, name)
		return exitUsage
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(errOut, "%s: canceled\n", name)
		return exitInterrupted
	}
	fmt.Fprintf(errOut, "%s: persistent state and recovery are not implemented in issue #3\n", name)
	return exitUnavailable
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	wasSet := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func isHelp(value string) bool {
	return value == "--help" || value == "-h"
}

func hasHelp(args []string) bool {
	for _, arg := range args {
		if isHelp(arg) {
			return true
		}
	}
	return false
}

func printRootHelp(out io.Writer) {
	fmt.Fprintln(out, "Runstead is a local, stateful and verifiable agent runtime.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Usage: runstead <command> [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  run       prepare an agent run (loop deferred)")
	fmt.Fprintln(out, "  inspect   inspect durable task state (placeholder)")
	fmt.Fprintln(out, "  resume    resume durable task state (placeholder)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Configuration precedence: flags > environment > defaults")
	fmt.Fprintf(out, "  %s, %s\n", config.EnvWorkspace, config.EnvLogLevel)
	fmt.Fprintln(out, "Use 'runstead <command> --help' for command-specific help.")
}

func printRunHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead run [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "The agent loop is not implemented in issue #3; this command fails clearly until the provider and loop issues land.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags:")
	fmt.Fprintln(out, "  --workspace PATH   workspace path (RUNSTEAD_WORKSPACE, default .)")
	fmt.Fprintln(out, "  --log-level LEVEL  debug, info, warn or error (RUNSTEAD_LOG_LEVEL, default info)")
}

func printPlaceholderHelp(out io.Writer, name string) {
	fmt.Fprintf(out, "Usage: runstead %s\n\n", name)
	fmt.Fprintf(out, "The %s command is an explicit placeholder; persistent state and recovery are not implemented in issue #3.\n", name)
	fmt.Fprintln(out, "No flags are currently supported.")
}
