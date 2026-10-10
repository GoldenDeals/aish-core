// Command aish-build builds an aish binary with the standard modules and any
// extra ones: modules are linked statically, so a different set of modules
// means a different binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
)

const (
	corePath  = "github.com/GoldenDeals/aish-core"
	goVersion = "1.27"
	fileMode  = 0o644
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var with, without listFlag
	flag.Var(&with, "with", "extra module: <module path>[@version][=<local dir>] (repeatable)")
	flag.Var(&without, "without", "standard module package to leave out, relative to modules/: llm/anthropic (repeatable)")
	out := flag.String("o", "./aish", "output binary")
	core := flag.String("core", "", "local aish-core checkout to build against instead of the published module")
	flag.Parse()

	o := options{With: with, Without: without, Core: *core}
	dest, err := filepath.Abs(*out)
	if err != nil {
		return fmt.Errorf("aish-build: %w", err)
	}
	dir, err := os.MkdirTemp("", "aish-build-")
	if err != nil {
		return fmt.Errorf("aish-build: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The standard modules are listed by modules/standard alone, so resolve
	// that package first, then rewrite main.go without the excluded ones.
	probe := o
	probe.Without = nil
	if err := write(ctx, dir, probe, []string{standardPackage}); err != nil {
		return err
	}
	std, err := goOutput(ctx, dir, "list", "-f", `{{join .Imports "\n"}}`, standardPackage)
	if err != nil {
		return err
	}
	if err := write(ctx, dir, o, strings.Fields(std)); err != nil {
		return err
	}

	return goRun(ctx, dir, "build", "-o", dest, ".")
}

// write generates the module in dir for the standard packages std and runs
// go mod tidy on it.
func write(ctx context.Context, dir string, o options, std []string) error {
	files, err := generate(o, std)
	if err != nil {
		return err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), fileMode); err != nil {
			return fmt.Errorf("aish-build: %w", err)
		}
	}

	return goRun(ctx, dir, "mod", "tidy")
}

func goOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // args are fixed by this program, not user input
	cmd.Dir, cmd.Stderr = dir, os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("aish-build: go %s: %w", strings.Join(args, " "), err)
	}

	return string(out), nil
}

func goRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // args are fixed by this program, not user input
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aish-build: go %s: %w", strings.Join(args, " "), err)
	}

	return nil
}
