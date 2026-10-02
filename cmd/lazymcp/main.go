// Command lazymcp wraps a stdio MCP server so it is only started when a client
// actually uses it.
//
//	lazymcp [OPTION]... -- COMMAND [ARG]...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/unstable-code/mcp-lazy/internal/proxy"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	// The flag package accepts -name and --name alike; the usage text below spells
	// them the GNU way, and -v/--verbose are the same flag.
	fs := flag.NewFlagSet("lazymcp", flag.ContinueOnError)
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "")
	var verbose bool
	fs.BoolVar(&verbose, "v", false, "")
	fs.BoolVar(&verbose, "verbose", false, "")
	showVersion := fs.Bool("version", false, "")
	// Errors and usage are printed here rather than by the flag package, so that
	// --help goes to stdout (exit 0) and mistakes go to stderr (exit 2).
	fs.SetOutput(io.Discard)
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "lazymcp: %v\n", err)
		usage(os.Stderr)
		return 2
	}
	if *showVersion {
		fmt.Println("lazymcp", version)
		return 0
	}
	command := fs.Args()
	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "lazymcp: missing server command")
		usage(os.Stderr)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	p := &proxy.Proxy{
		Command: command,
		Cache:   proxy.OpenCache(*cacheDir, command),
		In:      os.Stdin,
		Out:     os.Stdout,
		Stderr:  os.Stderr,
		Log:     log.New(os.Stderr, "lazymcp: ", 0),
		Verbose: verbose,
	}
	err := p.Run(ctx)
	var exit *proxy.ExitError
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return 0
	case errors.As(err, &exit):
		return exit.Code
	default:
		fmt.Fprintln(os.Stderr, "lazymcp:", err)
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `Usage: lazymcp [OPTION]... -- COMMAND [ARG]...
Start the stdio MCP server COMMAND only when a client first needs it.

Options:
      --cache-dir DIR  keep recorded server answers in DIR; empty disables the cache
                       (default %q)
  -v, --verbose        log cache hits and recordings to stderr
  -h, --help           show this help and exit
      --version        print the version and exit
`, defaultCacheDir())
}

func defaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "lazymcp")
}
