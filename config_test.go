package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadConfigFile(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
bridge_dev: "br-provider"
vrf_name: "vrf-test"
veth_nexthop: "169.254.0.2"
network_cidr: "192.0.2.0/24"
gateway_port: "cr-lrp-abc123"
reconcile_interval: "30s"
log_level: "debug"
cleanup_on_shutdown: false
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	fc, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error: %v", err)
	}

	if fc.OVNSBRemote != "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", fc.OVNSBRemote, "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642")
	}
	if fc.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", fc.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if fc.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", fc.BridgeDev, "br-provider")
	}
	if fc.VRFName != "vrf-test" {
		t.Errorf("VRFName = %q, want %q", fc.VRFName, "vrf-test")
	}
	if fc.VethNexthop != "169.254.0.2" {
		t.Errorf("VethNexthop = %q, want %q", fc.VethNexthop, "169.254.0.2")
	}
	if len(fc.NetworkCIDR) != 1 || fc.NetworkCIDR[0] != "192.0.2.0/24" {
		t.Errorf("NetworkCIDR = %v, want [192.0.2.0/24]", fc.NetworkCIDR)
	}
	if fc.GatewayPort != "cr-lrp-abc123" {
		t.Errorf("GatewayPort = %q, want %q", fc.GatewayPort, "cr-lrp-abc123")
	}
	if fc.ReconcileInterval != "30s" {
		t.Errorf("ReconcileInterval = %q, want %q", fc.ReconcileInterval, "30s")
	}
	if fc.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", fc.LogLevel, "debug")
	}
	if fc.CleanupOnShutdown == nil || *fc.CleanupOnShutdown != false {
		t.Errorf("CleanupOnShutdown = %v, want false", fc.CleanupOnShutdown)
	}
}

func TestReadConfigFileNotFound(t *testing.T) {
	_, err := readConfigFile("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestReadConfigFileInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	_, err := readConfigFile(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestReadConfigFileNetworkCIDRList(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
network_cidr:
  - "192.0.2.0/24"
  - "198.51.100.0/24"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	fc, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error: %v", err)
	}

	if len(fc.NetworkCIDR) != 2 {
		t.Fatalf("NetworkCIDR length = %d, want 2", len(fc.NetworkCIDR))
	}
	if fc.NetworkCIDR[0] != "192.0.2.0/24" || fc.NetworkCIDR[1] != "198.51.100.0/24" {
		t.Errorf("NetworkCIDR = %v, want [192.0.2.0/24 198.51.100.0/24]", fc.NetworkCIDR)
	}
}

func TestApplyEnvConfigMultipleCIDRs(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_ROUTE_NETWORK_CIDR", "10.0.0.0/24,172.16.0.0/12")
	applyEnvConfig(&cfg)

	if len(cfg.NetworkCIDRs) != 2 {
		t.Fatalf("NetworkCIDRs length = %d, want 2", len(cfg.NetworkCIDRs))
	}
	if cfg.NetworkCIDRs[0] != "10.0.0.0/24" || cfg.NetworkCIDRs[1] != "172.16.0.0/12" {
		t.Errorf("NetworkCIDRs = %v, want [10.0.0.0/24 172.16.0.0/12]", cfg.NetworkCIDRs)
	}
}

func TestApplyFileConfig(t *testing.T) {
	cfg := Config{
		BridgeDev:         "br-ex",
		VRFName:           "vrf-provider",
		VethNexthop:       "169.254.0.1",
		ReconcileInterval: 60 * time.Second,
		LogLevel:          "info",
	}

	fc := configFile{
		OVNSBRemote:       "tcp:10.0.0.1:6642",
		OVNNBRemote:       "tcp:10.0.0.1:6641",
		BridgeDev:         "br-provider",
		ReconcileInterval: "30s",
	}

	applyFileConfig(&cfg, &fc)

	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", cfg.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
	// Unchanged fields keep defaults.
	if cfg.VRFName != "vrf-provider" {
		t.Errorf("VRFName = %q, want %q", cfg.VRFName, "vrf-provider")
	}
	if cfg.VethNexthop != "169.254.0.1" {
		t.Errorf("VethNexthop = %q, want %q", cfg.VethNexthop, "169.254.0.1")
	}
	if cfg.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 30*time.Second)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestApplyFileConfigEmptyFieldsNoOverride(t *testing.T) {
	cfg := Config{
		BridgeDev: "br-ex",
		LogLevel:  "info",
	}

	fc := configFile{} // All empty.

	applyFileConfig(&cfg, &fc)

	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q (should not be overridden)", cfg.BridgeDev, "br-ex")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q (should not be overridden)", cfg.LogLevel, "info")
	}
}

func TestApplyEnvConfig(t *testing.T) {
	cfg := Config{
		BridgeDev: "br-ex",
		LogLevel:  "info",
	}

	t.Setenv("OVN_ROUTE_OVN_SB_REMOTE", "tcp:10.0.0.99:6642")
	t.Setenv("OVN_ROUTE_LOG_LEVEL", "debug")
	t.Setenv("OVN_ROUTE_RECONCILE_INTERVAL", "5m")
	t.Setenv("OVN_ROUTE_NETWORK_CIDR", "10.0.0.0/24")
	t.Setenv("OVN_ROUTE_GATEWAY_PORT", "cr-lrp-test")

	applyEnvConfig(&cfg)

	if cfg.OVNSBRemote != "tcp:10.0.0.99:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.99:6642")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.ReconcileInterval != 5*time.Minute {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 5*time.Minute)
	}
	if len(cfg.NetworkCIDRs) != 1 || cfg.NetworkCIDRs[0] != "10.0.0.0/24" {
		t.Errorf("NetworkCIDRs = %v, want [10.0.0.0/24]", cfg.NetworkCIDRs)
	}
	if cfg.GatewayPort != "cr-lrp-test" {
		t.Errorf("GatewayPort = %q, want %q", cfg.GatewayPort, "cr-lrp-test")
	}
	// Unchanged.
	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q (should not be overridden)", cfg.BridgeDev, "br-ex")
	}
}

func TestApplyEnvConfigInvalidDuration(t *testing.T) {
	cfg := Config{
		ReconcileInterval: 60 * time.Second,
	}

	t.Setenv("OVN_ROUTE_RECONCILE_INTERVAL", "notaduration")

	applyEnvConfig(&cfg)

	if cfg.ReconcileInterval != 60*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v (should not be overridden by invalid value)", cfg.ReconcileInterval, 60*time.Second)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-ex")
	}
	if cfg.VRFName != "vrf-provider" {
		t.Errorf("VRFName = %q, want %q", cfg.VRFName, "vrf-provider")
	}
	if cfg.VethNexthop != "169.254.0.1" {
		t.Errorf("VethNexthop = %q, want %q", cfg.VethNexthop, "169.254.0.1")
	}
	if cfg.BridgeIP != "169.254.169.254" {
		t.Errorf("BridgeIP = %q, want %q", cfg.BridgeIP, "169.254.169.254")
	}
	if cfg.ReconcileInterval != 60*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 60*time.Second)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if !cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be true by default")
	}
	if cfg.FRRPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "ANNOUNCED-NETWORKS")
	}
	// VethLeakEnabled was overridden to false for this test; check other defaults.
	if cfg.VethLeakTableID != 200 {
		t.Errorf("VethLeakTableID = %d, want 200", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 2000 {
		t.Errorf("VethLeakRulePriority = %d, want 2000", cfg.VethLeakRulePriority)
	}
}

func TestLoadConfigVethLeakEnabledByDefault(t *testing.T) {
	// VethLeakEnabled defaults to true and requires network-cidr.
	cfg, err := loadConfig([]string{"--network-cidr", "10.0.0.0/24"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be true by default")
	}
	if cfg.VethProviderIP != "169.254.0.2" {
		t.Errorf("VethProviderIP = %q, want %q (auto-computed from default nexthop)", cfg.VethProviderIP, "169.254.0.2")
	}
}

func TestLoadConfigCLIFlags(t *testing.T) {
	args := []string{
		"--ovn-sb-remote", "tcp:10.0.0.1:6642",
		"--ovn-nb-remote", "tcp:10.0.0.1:6641",
		"--bridge-dev", "br-provider",
		"--network-cidr", "10.0.0.0/24",
		"--gateway-port", "cr-lrp-test",
	}
	cfg, err := loadConfig(args)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", cfg.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
	if len(cfg.NetworkCIDRs) != 1 || cfg.NetworkCIDRs[0] != "10.0.0.0/24" {
		t.Errorf("NetworkCIDRs = %v, want [10.0.0.0/24]", cfg.NetworkCIDRs)
	}
	if len(cfg.NetworkFilters) != 1 {
		t.Fatal("NetworkFilters should have 1 entry when CIDR is set")
	}
	if cfg.GatewayPort != "cr-lrp-test" {
		t.Errorf("GatewayPort = %q, want %q", cfg.GatewayPort, "cr-lrp-test")
	}
}

func TestLoadConfigDryRunFlag(t *testing.T) {
	cfg, err := loadConfig([]string{"--dry-run"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true when --dry-run is set")
	}
}

func TestLoadConfigDryRunDefault(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.DryRun {
		t.Error("DryRun should be false by default")
	}
}

func TestApplyEnvConfigDryRun(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_ROUTE_DRY_RUN", "true")
	applyEnvConfig(&cfg)
	if !cfg.DryRun {
		t.Error("DryRun should be true when OVN_ROUTE_DRY_RUN=true")
	}
}

func TestApplyFileConfigDryRun(t *testing.T) {
	cfg := Config{}
	dryRun := true
	fc := configFile{DryRun: &dryRun}
	applyFileConfig(&cfg, &fc)
	if !cfg.DryRun {
		t.Error("DryRun should be true when set in config file")
	}
}

func TestLoadConfigCleanupOnShutdownDefault(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be true by default")
	}
}

func TestLoadConfigCleanupOnShutdownDisabledViaCLI(t *testing.T) {
	cfg, err := loadConfig([]string{"--cleanup-on-shutdown=false"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when --cleanup-on-shutdown=false is set")
	}
}

func TestApplyEnvConfigCleanupOnShutdownFalse(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	t.Setenv("OVN_ROUTE_CLEANUP_ON_SHUTDOWN", "false")
	applyEnvConfig(&cfg)
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when OVN_ROUTE_CLEANUP_ON_SHUTDOWN=false")
	}
}

func TestApplyEnvConfigCleanupOnShutdownZero(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	t.Setenv("OVN_ROUTE_CLEANUP_ON_SHUTDOWN", "0")
	applyEnvConfig(&cfg)
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when OVN_ROUTE_CLEANUP_ON_SHUTDOWN=0")
	}
}

func TestApplyFileConfigCleanupOnShutdown(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	cleanup := false
	fc := configFile{CleanupOnShutdown: &cleanup}
	applyFileConfig(&cfg, &fc)
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when set to false in config file")
	}
}

func TestApplyFileConfigCleanupOnShutdownNil(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	fc := configFile{} // CleanupOnShutdown is nil
	applyFileConfig(&cfg, &fc)
	if !cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should remain true when not set in config file")
	}
}

func TestLoadConfigWithFile(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
bridge_dev: "br-provider"
veth_leak_enabled: false
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
}

func TestLoadConfigCLIOverridesFile(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
bridge_dev: "br-provider"
veth_leak_enabled: false
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path, "--bridge-dev", "br-custom"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.BridgeDev != "br-custom" {
		t.Errorf("BridgeDev = %q, want %q (CLI should override file)", cfg.BridgeDev, "br-custom")
	}
	// File values should still apply for non-overridden fields.
	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
}

func TestLoadConfigVersionFlag(t *testing.T) {
	_, err := loadConfig([]string{"--version"})
	if !errors.Is(err, errVersionRequested) {
		t.Errorf("expected errVersionRequested, got: %v", err)
	}
}

func TestLoadConfigInvalidNetworkCIDR(t *testing.T) {
	_, err := loadConfig([]string{"--network-cidr", "not-a-cidr"})
	if err == nil {
		t.Error("expected error for invalid CIDR")
	}
}

func TestLoadConfigInvalidVethNexthop(t *testing.T) {
	_, err := loadConfig([]string{"--veth-nexthop", "not-an-ip"})
	if err == nil {
		t.Error("expected error for invalid veth-nexthop")
	}
}

func TestLoadConfigInvalidVRFName(t *testing.T) {
	_, err := loadConfig([]string{"--vrf-name", "bad name; drop"})
	if err == nil {
		t.Error("expected error for invalid VRF name")
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			"valid defaults",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider"},
			false,
		},
		{
			"valid with single CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"10.0.0.0/24"}},
			false,
		},
		{
			"valid with multiple CIDRs",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"10.0.0.0/24", "172.16.0.0/12"}},
			false,
		},
		{
			"invalid nexthop",
			Config{VethNexthop: "bad", VRFName: "vrf-provider"},
			true,
		},
		{
			"invalid VRF name",
			Config{VethNexthop: "169.254.0.1", VRFName: "bad name"},
			true,
		},
		{
			"invalid CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"bad"}},
			true,
		},
		{
			"one valid one invalid CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"10.0.0.0/24", "bad"}},
			true,
		},
		{
			"valid route table ID",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 100},
			false,
		},
		{
			"route table ID zero (main table)",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 0},
			false,
		},
		{
			"route table ID max",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 252},
			false,
		},
		{
			"route table ID too high",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 253},
			true,
		},
		{
			"route table ID negative",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: -1},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigRouteTableIDCLI(t *testing.T) {
	cfg, err := loadConfig([]string{"--route-table-id", "100"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.RouteTableID != 100 {
		t.Errorf("RouteTableID = %d, want 100", cfg.RouteTableID)
	}
}

func TestLoadConfigRouteTableIDInvalid(t *testing.T) {
	_, err := loadConfig([]string{"--route-table-id", "253"})
	if err == nil {
		t.Error("expected error for route-table-id 253")
	}
}

func TestApplyEnvConfigRouteTableID(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_ROUTE_ROUTE_TABLE_ID", "42")
	applyEnvConfig(&cfg)

	if cfg.RouteTableID != 42 {
		t.Errorf("RouteTableID = %d, want 42", cfg.RouteTableID)
	}
}

func TestEffectiveNetworkFilters(t *testing.T) {
	_, manual, _ := net.ParseCIDR("10.0.0.0/24")
	_, discovered, _ := net.ParseCIDR("198.51.100.0/24")

	t.Run("manual takes precedence", func(t *testing.T) {
		got := effectiveNetworkFilters([]*net.IPNet{manual}, []*net.IPNet{discovered})
		if len(got) != 1 || got[0].String() != "10.0.0.0/24" {
			t.Errorf("expected manual, got %v", got)
		}
	})

	t.Run("discovered when no manual", func(t *testing.T) {
		got := effectiveNetworkFilters(nil, []*net.IPNet{discovered})
		if len(got) != 1 || got[0].String() != "198.51.100.0/24" {
			t.Errorf("expected discovered, got %v", got)
		}
	})

	t.Run("nil when both empty", func(t *testing.T) {
		got := effectiveNetworkFilters(nil, nil)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})
}

func TestIsValidIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"vrf-provider", true},
		{"vrf_provider", true},
		{"vrf.provider", true},
		{"VRF123", true},
		{"", false},
		{"bad name", false},
		{"bad;name", false},
		{"bad$name", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isValidIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("isValidIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadConfigVethLeakWithoutNetworkCIDR(t *testing.T) {
	// Veth leak no longer requires network-cidr — networks are auto-discovered from OVN.
	cfg, err := loadConfig([]string{"--veth-leak-enabled"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be true")
	}
}

func TestLoadConfigVethLeakDisabledWithoutNetworkCIDR(t *testing.T) {
	cfg, err := loadConfig([]string{"--veth-leak-enabled=false"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be false")
	}
}

func TestLoadConfigVethLeakAutoProviderIP(t *testing.T) {
	cfg, err := loadConfig([]string{
		"--veth-nexthop", "169.254.0.1",
		"--network-cidr", "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.VethProviderIP != "169.254.0.2" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.2")
	}
}

func TestLoadConfigVethLeakExplicitProviderIP(t *testing.T) {
	cfg, err := loadConfig([]string{
		"--veth-provider-ip", "169.254.0.10",
		"--network-cidr", "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.VethProviderIP != "169.254.0.10" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.10")
	}
}

func TestLoadConfigVethLeakTableIDConflict(t *testing.T) {
	_, err := loadConfig([]string{
		"--route-table-id", "200",
		"--veth-leak-table-id", "200",
		"--network-cidr", "10.0.0.0/24",
	})
	if err == nil {
		t.Error("expected error when veth-leak-table-id equals route-table-id")
	}
}

func TestLoadConfigVethLeakTableIDInvalid(t *testing.T) {
	_, err := loadConfig([]string{
		"--veth-leak-table-id", "0",
		"--network-cidr", "10.0.0.0/24",
	})
	if err == nil {
		t.Error("expected error for veth-leak-table-id 0")
	}
}

func TestApplyEnvConfigVethLeak(t *testing.T) {
	cfg := Config{VethLeakEnabled: true}
	t.Setenv("OVN_ROUTE_VETH_LEAK_ENABLED", "false")
	t.Setenv("OVN_ROUTE_VETH_PROVIDER_IP", "169.254.0.5")
	t.Setenv("OVN_ROUTE_VETH_LEAK_TABLE_ID", "201")
	t.Setenv("OVN_ROUTE_VETH_LEAK_RULE_PRIORITY", "3000")
	applyEnvConfig(&cfg)

	if cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be false")
	}
	if cfg.VethProviderIP != "169.254.0.5" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.5")
	}
	if cfg.VethLeakTableID != 201 {
		t.Errorf("VethLeakTableID = %d, want 201", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 3000 {
		t.Errorf("VethLeakRulePriority = %d, want 3000", cfg.VethLeakRulePriority)
	}
}

func TestApplyFileConfigVethLeak(t *testing.T) {
	cfg := Config{VethLeakEnabled: true, VethLeakTableID: 200, VethLeakRulePriority: 2000}
	enabled := false
	tableID := 201
	prio := 3000
	fc := configFile{
		VethLeakEnabled:      &enabled,
		VethProviderIP:       "169.254.0.5",
		VethLeakTableID:      &tableID,
		VethLeakRulePriority: &prio,
	}
	applyFileConfig(&cfg, &fc)

	if cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be false")
	}
	if cfg.VethProviderIP != "169.254.0.5" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.5")
	}
	if cfg.VethLeakTableID != 201 {
		t.Errorf("VethLeakTableID = %d, want 201", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 3000 {
		t.Errorf("VethLeakRulePriority = %d, want 3000", cfg.VethLeakRulePriority)
	}
}

func TestLoadConfigVethLeakYAML(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
network_cidr: "10.0.0.0/24"
veth_leak_enabled: true
veth_provider_ip: "169.254.0.5"
veth_leak_table_id: 201
veth_leak_rule_priority: 3000
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be true")
	}
	if cfg.VethProviderIP != "169.254.0.5" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.5")
	}
	if cfg.VethLeakTableID != 201 {
		t.Errorf("VethLeakTableID = %d, want 201", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 3000 {
		t.Errorf("VethLeakRulePriority = %d, want 3000", cfg.VethLeakRulePriority)
	}
}

func TestLoadConfigFRRPrefixListCLI(t *testing.T) {
	cfg, err := loadConfig([]string{"--frr-prefix-list", "ANNOUNCED-NETWORKS"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.FRRPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "ANNOUNCED-NETWORKS")
	}
}

func TestLoadConfigFRRPrefixListInvalid(t *testing.T) {
	_, err := loadConfig([]string{"--frr-prefix-list", "bad name; drop"})
	if err == nil {
		t.Error("expected error for invalid frr-prefix-list name")
	}
}

func TestLoadConfigStaleChassisGracePeriodDefault(t *testing.T) {
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 5*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 5*time.Minute)
	}
}

func TestLoadConfigStaleChassisGracePeriodCLI(t *testing.T) {
	cfg, err := loadConfig([]string{"--stale-chassis-grace-period", "10m"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 10*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 10*time.Minute)
	}
}

func TestLoadConfigStaleChassisGracePeriodDisabled(t *testing.T) {
	cfg, err := loadConfig([]string{"--stale-chassis-grace-period", "0s"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 0 {
		t.Errorf("StaleChassisGracePeriod = %v, want 0 (disabled)", cfg.StaleChassisGracePeriod)
	}
}

func TestApplyEnvConfigStaleChassisGracePeriod(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	t.Setenv("OVN_ROUTE_STALE_CHASSIS_GRACE_PERIOD", "3m")
	applyEnvConfig(&cfg)
	if cfg.StaleChassisGracePeriod != 3*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 3*time.Minute)
	}
}

func TestApplyFileConfigStaleChassisGracePeriod(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	fc := configFile{StaleChassisGracePeriod: "2m"}
	applyFileConfig(&cfg, &fc)
	if cfg.StaleChassisGracePeriod != 2*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 2*time.Minute)
	}
}

func TestApplyFileConfigStaleChassisGracePeriodEmpty(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	fc := configFile{}
	applyFileConfig(&cfg, &fc)
	if cfg.StaleChassisGracePeriod != 5*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v (should keep default)", cfg.StaleChassisGracePeriod, 5*time.Minute)
	}
}

func TestLoadConfigStaleChassisGracePeriodYAML(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
stale_chassis_grace_period: "7m"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 7*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 7*time.Minute)
	}
}

func TestValidateConfigStaleChassisGracePeriodNegative(t *testing.T) {
	cfg := Config{
		VethNexthop: "169.254.0.1",
		VRFName:     "vrf-provider",
		StaleChassisGracePeriod: -1 * time.Minute,
	}
	err := validateConfig(&cfg)
	if err == nil {
		t.Error("expected error for negative stale-chassis-grace-period")
	}
}

func TestApplyEnvConfigFRRPrefixList(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_ROUTE_FRR_PREFIX_LIST", "MY-LIST")
	applyEnvConfig(&cfg)
	if cfg.FRRPrefixList != "MY-LIST" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "MY-LIST")
	}
}

func TestApplyFileConfigFRRPrefixList(t *testing.T) {
	cfg := Config{}
	fc := configFile{FRRPrefixList: "FILE-LIST"}
	applyFileConfig(&cfg, &fc)
	if cfg.FRRPrefixList != "FILE-LIST" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "FILE-LIST")
	}
}

func TestNextIPInSubnet(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"169.254.0.1", "169.254.0.2"},
		{"169.254.0.254", "169.254.0.255"},
		{"169.254.0.255", "169.254.1.0"},
		{"10.0.0.0", "10.0.0.1"},
		{"255.255.255.255", "0.0.0.0"}, // wraps around
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := nextIPInSubnet(net.ParseIP(tt.input))
			if got.String() != tt.want {
				t.Errorf("nextIPInSubnet(%s) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}
