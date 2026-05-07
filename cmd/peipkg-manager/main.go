// Command peipkg-manager runs the build farm: watch upstreams for new
// versions, build packages with peipkg-build, ingest with peipkg-repo,
// optionally upload to a remote object store. Designed to be installed
// as a systemd service on a long-running host.
//
// Usage:
//
//	peipkg-manager --config /etc/peipkg-manager/config.toml
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/peios/peipkg-manager/internal/config"
	"github.com/peios/peipkg-manager/internal/manager"
)

const defaultConfigPath = "/etc/peipkg-manager/config.toml"

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to peipkg-config.toml")
	logLevel := flag.String("log-level", "info", "log verbosity: debug, info, warn, error")
	once := flag.Bool("once", false, "run a single poll/build/publish sweep and exit (cron/CI mode); default is the long-running daemon")
	flag.Parse()

	logger, err := newLogger(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "peipkg-manager:", err)
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	mgr, err := manager.New(cfg, manager.Options{Logger: logger})
	if err != nil {
		logger.Error("construct manager", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mode := "daemon"
	runFn := mgr.Run
	if *once {
		mode = "once"
		runFn = mgr.RunOnce
	}

	// SIGHUP triggers a recipe-roster reload without restarting the
	// process. Only meaningful for daemon mode — in --once mode the
	// process exits before the signal could ever fire.
	if !*once {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		go func() {
			for {
				select {
				case <-ctx.Done():
					signal.Stop(hupCh)
					return
				case <-hupCh:
					logger.Info("received SIGHUP; reloading recipes")
					mgr.Reload()
				}
			}
		}()
	}

	logger.Info("peipkg-manager starting", "config", *configPath, "farm_id", cfg.Manager.ID, "mode", mode)
	if err := runFn(ctx); err != nil && err != context.Canceled {
		logger.Error("manager exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("peipkg-manager stopped")
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (want one of: debug, info, warn, error)", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}
