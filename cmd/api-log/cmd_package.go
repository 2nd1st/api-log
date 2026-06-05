package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/2nd1st/api-log/internal/config"
	"github.com/2nd1st/api-log/internal/exporter"
	"github.com/2nd1st/api-log/internal/store/sqlite"
)

// runPackage executes the `api-log package` subcommand. Builds the same
// zip shape as POST /api/export against the data dir on disk — JSONL
// + media + agent/CLAUDE.md + jq-cheatsheet + README — without
// requiring the proxy/API listeners to be running. Intended for two
// jobs:
//
//  1. Offline analysis. An operator hands the zip to an agent
//     (Claude / Codex / a local LLM) for diagnosis; the agent reads
//     agent/CLAUDE.md to know the JSONL shape.
//  2. Cold-storage handoff. A cron pipes a recent window to S3 / R2
//     / a NAS by passing -out=- (stdout) into the operator's sink of
//     choice — no archive sink plugin needed.
//
// The subcommand opens the SQLite index alongside any running api-log
// process (WAL mode handles concurrent readers cleanly per
// ARCHITECTURE § 4); it does NOT pass a storage.Coordinator to the
// exporter — without one, the exporter reads JSONL buckets without
// lease arbitration. That's safe because the buckets are append-only
// and a retention delete that races us will only manifest as a clean
// "file vanished" skip inside exporter.writeGroupEntry. The tradeoff
// is intentional: the alternative would require the subcommand to
// start a Coordinator goroutine that has no business running outside
// the long-lived proxy process.
//
// The byte cap is deliberately disabled (byteHardCap=0). The HTTP
// surface ships a 2 GiB pre-flight to protect a curl-driven export
// from blowing memory; the CLI surface is the operator's own machine,
// where "I asked for the whole quarter, give me the whole quarter" is
// the contract.
func runPackage(args []string) error {
	fs := flag.NewFlagSet("package", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: api-log package [flags]

Build an offline zip of recorded traces — same shape as POST /api/export
(JSONL + media + agent/CLAUDE.md). Reads storage.data_dir from -config
(falls back to env / defaults).

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Examples:
  api-log package -config /etc/api-log.yaml -out ./traces.zip
  api-log package -config /etc/api-log.yaml -from 2026-06-01 -to 2026-06-08 -out ./week.zip
  api-log package -config /etc/api-log.yaml -path-prefix /v1 -out - | aws s3 cp - s3://bucket/key.zip
`)
	}

	var (
		configPath string
		outPath    string
		fromStr    string
		toStr      string
		path       string
		pathPrefix string
		model      string
		keyPrefix  string
		limit      int
	)
	fs.StringVar(&configPath, "config", "", "path to api-log.yaml (empty = defaults + env)")
	fs.StringVar(&outPath, "out", "", "output zip path; \"-\" writes to stdout (default: ./api-log-package-<RFC3339>.zip)")
	fs.StringVar(&fromStr, "from", "", "lower ts_start bound; RFC3339 or YYYY-MM-DD (inclusive)")
	fs.StringVar(&toStr, "to", "", "upper ts_start bound; RFC3339 or YYYY-MM-DD (exclusive)")
	fs.StringVar(&path, "path", "", "exact request path match")
	fs.StringVar(&pathPrefix, "path-prefix", "", "request path prefix (LIKE prefix%)")
	fs.StringVar(&model, "model", "", "exact model match")
	fs.StringVar(&keyPrefix, "key-prefix", "", "key_hash 8- or 16-char prefix")
	fs.IntVar(&limit, "limit", 100000, "max rows; 0 = no limit (caller owns the disk math)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if pathPrefix != "" && path != "" {
		return errors.New("-path and -path-prefix are mutually exclusive")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config validate: %w", err)
	}

	from, err := parsePackageTime(fromStr)
	if err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	to, err := parsePackageTime(toStr)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}

	sqlitePath := filepath.Join(cfg.Storage.DataDir, "index.sqlite")
	store, err := sqlite.Open(sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer func() { _ = store.Close() }()

	filters := sqlite.ListFilters{
		Since:         from,
		Until:         to,
		Path:          path,
		PathPrefix:    pathPrefix,
		Model:         model,
		KeyHashPrefix: keyPrefix,
		Limit:         limit,
	}

	out, closeOut, err := openPackageOutput(outPath)
	if err != nil {
		return err
	}
	defer closeOut()

	// Honor Ctrl-C / SIGTERM by cancelling the exporter context — a half-
	// written zip on disk is the operator's problem to delete, but the
	// signal lets them abort a wrong-window export without kill -9.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// byteHardCap=0: CLI bypasses the 2 GiB pre-flight that protects the
	// HTTP surface. coord=nil: no lease arbitration, see the doc comment
	// above for the safety argument.
	if err := exporter.WriteZip(ctx, out, store, cfg.Storage.DataDir, filters, 0, nil); err != nil {
		return fmt.Errorf("write zip: %w", err)
	}
	return nil
}

// parsePackageTime accepts the two shapes an operator is realistically
// going to type at a shell: full RFC3339 (`2026-06-08T00:00:00Z`) and
// bare-date (`2026-06-08`, interpreted as UTC midnight). Returns the
// zero time on empty input so the caller can leave the bound open.
func parsePackageTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 or YYYY-MM-DD, got %q", s)
}

// openPackageOutput resolves the -out flag into an io.Writer + a close
// hook. "-" writes to stdout; empty defaults to
// ./api-log-package-<RFC3339-Z>.zip in the working directory.
func openPackageOutput(outPath string) (io.Writer, func(), error) {
	if outPath == "-" {
		return os.Stdout, func() {}, nil
	}
	if outPath == "" {
		// Filename uses RFC3339 with `:` swapped to `-` so the result is
		// portable across Windows / S3 / NAS shares that reject colons in
		// object keys. The conversion is reversible by an operator who
		// needs the source timestamp.
		stamp := strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339), ":", "-")
		outPath = "api-log-package-" + stamp + ".zip"
	}
	// 0o600: zips of recorded JSONL carry raw bearer tokens via the
	// captured request headers — keep the same perms the writer applies
	// to the source JSONL files. Operators who need broader access chmod
	// after the fact, surfacing the choice.
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", strconv.Quote(outPath), err)
	}
	closer := func() {
		_ = f.Close()
	}
	// Mirror the runtime log line shape (key=value) so an operator who
	// pipes stderr through the same parser gets a consistent record.
	fmt.Fprintf(os.Stderr, "api-log package: writing zip out=%s\n", outPath)
	return f, closer, nil
}
