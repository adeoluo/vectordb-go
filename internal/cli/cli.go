// Package cli implements the vectordb command line: serve and bench.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/adeoluo/vectordb-go/internal/db"
	"github.com/adeoluo/vectordb-go/internal/server"
	"github.com/adeoluo/vectordb-go/internal/wal"
)

// Version is set at build time with -ldflags "-X .../cli.Version=v0.1.0".
var Version = "dev"

const usage = `vectordb: a persistent, concurrent HNSW vector database

Usage:
  vectordb serve --data-dir DIR [flags]   run the HTTP server
  vectordb bench [flags]                  measure build time, recall and latency
  vectordb load --url URL [flags]         drive a running server over HTTP
  vectordb version                        print the version

Run "vectordb <command> -h" for the flags of a command.
`

// Run executes the command line in args and returns the process exit code.
// ctx is cancelled to request a graceful shutdown.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx, args[1:], stderr)
	case "bench":
		err = bench(args[1:], stdout, stderr)
	case "load":
		err = load(args[1:], stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, versionString())
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "vectordb %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func versionString() string {
	v := Version
	if info, ok := debug.ReadBuildInfo(); ok && v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	return "vectordb " + v
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "directory for collections, logs and snapshots (required)")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	addrFile := fs.String("addr-file", "", "write the bound listen address to this file once ready")
	fsync := fs.String("fsync", "always", "WAL fsync policy: always, interval or never")
	fsyncInterval := fs.Duration("fsync-interval", 100*time.Millisecond, "fsync period for --fsync interval")
	segmentSize := fs.Int64("wal-segment-bytes", 64<<20, "rotate WAL segments at this size")
	snapInterval := fs.Duration("snapshot-interval", 5*time.Minute, "snapshot when this much time has passed since the last one (0 disables)")
	snapBytes := fs.Int64("snapshot-wal-bytes", 256<<20, "snapshot after this many WAL bytes (0 disables)")
	snapOnClose := fs.Bool("snapshot-on-close", true, "write a snapshot during graceful shutdown")
	shutdownTimeout := fs.Duration("shutdown-timeout", 15*time.Second, "time allowed for in-flight requests during shutdown")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		fs.Usage()
		return errors.New("--data-dir is required")
	}
	policy, err := wal.ParseSyncPolicy(*fsync)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	d, err := db.Open(db.Options{
		DataDir:          *dataDir,
		Sync:             policy,
		SyncInterval:     *fsyncInterval,
		SegmentSize:      *segmentSize,
		SnapshotInterval: *snapInterval,
		SnapshotWALBytes: *snapBytes,
		SnapshotOnClose:  *snapOnClose,
		Logger:           logger,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		d.Close()
		return err
	}
	srv := &http.Server{
		Handler:           server.New(d, logger),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	bound := ln.Addr().String()
	logger.Info("listening", "addr", bound, "data_dir", *dataDir, "fsync", policy.String(), "version", versionString())
	if *addrFile != "" {
		tmp := *addrFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(bound), 0o644); err == nil {
			os.Rename(tmp, *addrFile)
		}
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errc:
		d.Close()
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(sctx)
	closeErr := d.Close()
	if err := errors.Join(shutdownErr, closeErr); err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

func splitInts(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(part), "%d", &v); err != nil || v <= 0 {
			return nil, fmt.Errorf("invalid integer list %q", s)
		}
		out = append(out, v)
	}
	return out, nil
}
