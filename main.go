package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var version = "dev"

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, errVersionRequested) {
			fmt.Println("ovn-route-agent", version)
			os.Exit(0)
		}
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	setupLogging(cfg.LogLevel)

	if cfg.OVNSBRemote == "" || cfg.OVNNBRemote == "" {
		slog.Error("OVN database remotes are required, set --ovn-sb-remote / --ovn-nb-remote, OVN_ROUTE_OVN_SB_REMOTE / OVN_ROUTE_OVN_NB_REMOTE, or use a config file (--config)")
		os.Exit(1)
	}

	slog.Info("ovn-route-agent starting",
		"version", version,
		"dry_run", cfg.DryRun,
		"cleanup_on_shutdown", cfg.CleanupOnShutdown,
		"ovn_sb_remote", cfg.OVNSBRemote,
		"ovn_nb_remote", cfg.OVNNBRemote,
		"bridge_dev", cfg.BridgeDev,
		"vrf_name", cfg.VRFName,
		"veth_nexthop", cfg.VethNexthop,
		"network_cidrs", cfg.NetworkCIDRs,
		"gateway_port", cfg.GatewayPort,
		"route_table_id", cfg.RouteTableID,
		"ovs_wrapper", cfg.OVSWrapper,
		"reconcile_interval", cfg.ReconcileInterval,
	)

	if cfg.DryRun {
		slog.Warn("running in dry-run mode, no routes will be added or removed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	agent, err := NewAgent(cfg)
	if err != nil {
		slog.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("ovn-route-agent stopped")
}

func setupLogging(level string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})
	slog.SetDefault(slog.New(handler))
}
