//go:build !linux

package main

import (
	"fmt"
	"log/slog"
)

func (rm *RouteManager) CheckBridgeDevice() error {
	if rm.dryRun {
		slog.Info("[dry-run] skipping bridge device check", "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) AddKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add kernel route", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) DelKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove kernel route", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) ListKernelRoutes() ([]string, error) {
	if rm.dryRun {
		return nil, nil
	}
	return nil, fmt.Errorf("kernel route management is only supported on Linux")
}
