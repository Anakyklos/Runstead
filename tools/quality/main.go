// Command quality-gates runs the Runstead CI quality gates.
//
// Each gate is deterministic and offline: it never touches a provider,
// the network or credentials. The errcheck gate shells out to the local
// Go toolchain (`go list -export`) for type-accurate analysis and reads
// only local package source and build-cache export data.
//
// Exit codes: 0 = all gates passed, 1 = a gate found violations,
// 2 = usage or tooling error.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const usageText = `usage: quality-gates <gate> [--root DIR]

gates:
  growth            bounded-growth guards for the Go source tree
  errcheck          type-accurate swallowed-error detection (non-test files)
  live-convention   opt-in live test convention (RUNSTEAD_LIVE_* + t.Skip)

--root DIR   repository root to analyze (default: current directory)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	gate := args[0]
	fs := flag.NewFlagSet("quality-gates "+gate, flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", "", "repository root to analyze (default: current directory)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	root := *rootFlag
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "quality-gates: determine working directory: %v\n", err)
			return 2
		}
		root = cwd
	}

	switch gate {
	case "growth":
		return runGrowthCLI(root, stdout, stderr)
	case "errcheck":
		return runErrcheckCLI(root, stdout, stderr)
	case "live-convention":
		return runLiveConventionCLI(root, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "quality-gates: unknown gate %q\n\n%s", gate, usageText)
		return 2
	}
}

func runGrowthCLI(root string, stdout, stderr io.Writer) int {
	limits, err := DefaultLimits()
	if err != nil {
		fmt.Fprintf(stderr, "quality-gates growth: %v\n", err)
		return 2
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		fmt.Fprintf(stderr, "quality-gates growth: %v\n", err)
		return 2
	}
	if len(violations) == 0 {
		fmt.Fprintln(stdout, "PASS: bounded-growth guards")
		return 0
	}
	for _, v := range violations {
		fmt.Fprintln(stdout, v.String())
	}
	fmt.Fprintf(stdout, "FAIL: %d bounded-growth violation(s)\n", len(violations))
	return 1
}

func runErrcheckCLI(root string, stdout, stderr io.Writer) int {
	allowlist, err := DefaultAllowlist()
	if err != nil {
		fmt.Fprintf(stderr, "quality-gates errcheck: %v\n", err)
		return 2
	}
	report, err := RunErrcheck(root, allowlist)
	if err != nil {
		fmt.Fprintf(stderr, "quality-gates errcheck: %v\n", err)
		return 2
	}
	if len(report.Findings) == 0 && len(report.Stale) == 0 {
		fmt.Fprintln(stdout, "PASS: no swallowed errors outside the allowlist")
		return 0
	}
	for _, f := range report.Findings {
		fmt.Fprintf(stdout, "swallowed error: %s\n", f)
	}
	for _, s := range report.Stale {
		fmt.Fprintf(stdout, "stale allowlist entry: %s\n", s)
	}
	n := len(report.Findings) + len(report.Stale)
	fmt.Fprintf(stdout, "FAIL: %d swallowed-error finding(s) (see tools/quality/errcheck.allowlist)\n", n)
	return 1
}

func runLiveConventionCLI(root string, stdout, stderr io.Writer) int {
	violations, err := RunLiveConvention(root)
	if err != nil {
		fmt.Fprintf(stderr, "quality-gates live-convention: %v\n", err)
		return 2
	}
	if len(violations) == 0 {
		fmt.Fprintln(stdout, "PASS: live tests are opt-in and skipped by default")
		return 0
	}
	for _, v := range violations {
		fmt.Fprintln(stdout, v)
	}
	fmt.Fprintf(stdout, "FAIL: %d live-test convention violation(s)\n", len(violations))
	return 1
}
