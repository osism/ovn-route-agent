package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var errVersionRequested = errors.New("version requested")

// StringOrSlice is a YAML type that accepts both a scalar string and a list of strings.
type StringOrSlice []string

func (s *StringOrSlice) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*s = []string{value.Value}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

// containedInAny returns true if ip is contained in any of the given networks.
func containedInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// splitAndTrim splits s by sep, trims whitespace, and returns non-empty parts.
func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// Config holds all runtime configuration for the agent.
type Config struct {
	OVNSBRemote       string
	OVNNBRemote       string
	BridgeDev         string
	VRFName           string
	VethNexthop       string
	NetworkCIDRs      []string
	NetworkFilters    []*net.IPNet
	GatewayPort       string
	RouteTableID      int
	OVSWrapper        string // e.g. "docker exec openvswitch_vswitchd" — prepended to ovs-vsctl/ovs-ofctl calls
	ReconcileInterval time.Duration
	LogLevel          string
	DryRun            bool
	CleanupOnShutdown bool
}

// configFile is the YAML representation of the config file.
type configFile struct {
	OVNSBRemote       string        `yaml:"ovn_sb_remote"`
	OVNNBRemote       string        `yaml:"ovn_nb_remote"`
	BridgeDev         string        `yaml:"bridge_dev"`
	VRFName           string        `yaml:"vrf_name"`
	VethNexthop       string        `yaml:"veth_nexthop"`
	NetworkCIDR       StringOrSlice `yaml:"network_cidr"`
	GatewayPort       string        `yaml:"gateway_port"`
	RouteTableID      *int          `yaml:"route_table_id"`
	OVSWrapper        string        `yaml:"ovs_wrapper"`
	ReconcileInterval string        `yaml:"reconcile_interval"`
	LogLevel          string        `yaml:"log_level"`
	DryRun            *bool         `yaml:"dry_run"`
	CleanupOnShutdown *bool         `yaml:"cleanup_on_shutdown"`
}

// loadConfig builds the configuration with the following priority
// (highest wins): CLI flags > environment variables > config file > defaults.
func loadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("ovn-route-agent", flag.ContinueOnError)

	var (
		configPath  = fs.String("config", os.Getenv("OVN_ROUTE_CONFIG"), "Path to YAML config file")
		showVersion = fs.Bool("version", false, "Print version and exit")
		fOVNSB      = fs.String("ovn-sb-remote", "", "OVN Southbound DB remote, comma-separated for cluster")
		fOVNNB      = fs.String("ovn-nb-remote", "", "OVN Northbound DB remote, comma-separated for cluster")
		fBridge     = fs.String("bridge-dev", "", "Provider bridge device for kernel routes")
		fVRF        = fs.String("vrf-name", "", "VRF name for FRR routes")
		fNexthop    = fs.String("veth-nexthop", "", "Nexthop IP for FRR static routes in the VRF")
		fCIDR       = fs.String("network-cidr", "", "Filter FIPs by CIDRs (comma-separated, e.g. 10.0.0.0/24,172.16.0.0/12), empty = all")
		fGWPort     = fs.String("gateway-port", "", "Chassisredirect port filter; empty = track all routers automatically")
		fTableID    = fs.Int("route-table-id", 0, "Routing table ID for FIP routes (1-252); 0 = main table")
		fOVSWrapper = fs.String("ovs-wrapper", "", "Command prefix for ovs-vsctl/ovs-ofctl (e.g. 'docker exec openvswitch_vswitchd')")
		fInterval   = fs.String("reconcile-interval", "", "Full reconciliation interval (e.g. 60s, 5m)")
		fLogLevel          = fs.String("log-level", "", "Log level (debug, info, warn, error)")
		fDryRun            = fs.Bool("dry-run", false, "Dry-run mode: connect and reconcile but only log what would be done")
		fCleanupOnShutdown = fs.Bool("cleanup-on-shutdown", true, "Remove all managed routes on shutdown (SIGINT/SIGTERM)")
	)

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if *showVersion {
		return Config{}, errVersionRequested
	}

	// Layer 0: defaults
	cfg := Config{
		BridgeDev:         "br-ex",
		VRFName:           "vrf-provider",
		VethNexthop:       "169.254.0.1",
		ReconcileInterval: 60 * time.Second,
		LogLevel:          "info",
		CleanupOnShutdown: true,
	}

	// Layer 1: config file
	if *configPath != "" {
		fc, err := readConfigFile(*configPath)
		if err != nil {
			return Config{}, fmt.Errorf("load config %s: %w", *configPath, err)
		}
		applyFileConfig(&cfg, &fc)
	}

	// Layer 2: environment variables
	applyEnvConfig(&cfg)

	// Layer 3: CLI flags (only explicitly set ones)
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "ovn-sb-remote":
			cfg.OVNSBRemote = *fOVNSB
		case "ovn-nb-remote":
			cfg.OVNNBRemote = *fOVNNB
		case "bridge-dev":
			cfg.BridgeDev = *fBridge
		case "vrf-name":
			cfg.VRFName = *fVRF
		case "veth-nexthop":
			cfg.VethNexthop = *fNexthop
		case "network-cidr":
			cfg.NetworkCIDRs = splitAndTrim(*fCIDR, ",")
		case "gateway-port":
			cfg.GatewayPort = *fGWPort
		case "route-table-id":
			cfg.RouteTableID = *fTableID
		case "ovs-wrapper":
			cfg.OVSWrapper = *fOVSWrapper
		case "reconcile-interval":
			if d, err := time.ParseDuration(*fInterval); err == nil {
				cfg.ReconcileInterval = d
			}
		case "log-level":
			cfg.LogLevel = *fLogLevel
		case "dry-run":
			cfg.DryRun = *fDryRun
		case "cleanup-on-shutdown":
			cfg.CleanupOnShutdown = *fCleanupOnShutdown
		}
	})

	// Validate configuration
	if err := validateConfig(&cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func validateConfig(cfg *Config) error {
	if net.ParseIP(cfg.VethNexthop) == nil {
		return fmt.Errorf("invalid veth-nexthop IP: %q", cfg.VethNexthop)
	}
	if cfg.VRFName != "" && !isValidIdentifier(cfg.VRFName) {
		return fmt.Errorf("invalid vrf-name: %q (only alphanumeric, hyphen, underscore, dot allowed)", cfg.VRFName)
	}
	if cfg.RouteTableID < 0 || cfg.RouteTableID > 252 {
		return fmt.Errorf("invalid route-table-id: %d (must be 0-252)", cfg.RouteTableID)
	}
	if len(cfg.NetworkCIDRs) > 0 {
		cfg.NetworkFilters = make([]*net.IPNet, 0, len(cfg.NetworkCIDRs))
		for _, cidrStr := range cfg.NetworkCIDRs {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				return fmt.Errorf("invalid network-cidr %q: %w", cidrStr, err)
			}
			cfg.NetworkFilters = append(cfg.NetworkFilters, cidr)
		}
	}
	return nil
}

// isValidIdentifier checks that a string contains only safe characters
// for use in shell commands (alphanumeric, hyphen, underscore, dot).
func isValidIdentifier(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return len(s) > 0
}

func readConfigFile(path string) (configFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return configFile{}, err
	}
	var fc configFile
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return configFile{}, err
	}
	return fc, nil
}

func applyFileConfig(cfg *Config, fc *configFile) {
	if fc.OVNSBRemote != "" {
		cfg.OVNSBRemote = fc.OVNSBRemote
	}
	if fc.OVNNBRemote != "" {
		cfg.OVNNBRemote = fc.OVNNBRemote
	}
	if fc.BridgeDev != "" {
		cfg.BridgeDev = fc.BridgeDev
	}
	if fc.VRFName != "" {
		cfg.VRFName = fc.VRFName
	}
	if fc.VethNexthop != "" {
		cfg.VethNexthop = fc.VethNexthop
	}
	if len(fc.NetworkCIDR) > 0 {
		cfg.NetworkCIDRs = []string(fc.NetworkCIDR)
	}
	if fc.GatewayPort != "" {
		cfg.GatewayPort = fc.GatewayPort
	}
	if fc.OVSWrapper != "" {
		cfg.OVSWrapper = fc.OVSWrapper
	}
	if fc.RouteTableID != nil {
		cfg.RouteTableID = *fc.RouteTableID
	}
	if fc.ReconcileInterval != "" {
		if d, err := time.ParseDuration(fc.ReconcileInterval); err == nil {
			cfg.ReconcileInterval = d
		}
	}
	if fc.LogLevel != "" {
		cfg.LogLevel = fc.LogLevel
	}
	if fc.DryRun != nil {
		cfg.DryRun = *fc.DryRun
	}
	if fc.CleanupOnShutdown != nil {
		cfg.CleanupOnShutdown = *fc.CleanupOnShutdown
	}
}

func applyEnvConfig(cfg *Config) {
	if v := os.Getenv("OVN_ROUTE_OVN_SB_REMOTE"); v != "" {
		cfg.OVNSBRemote = v
	}
	if v := os.Getenv("OVN_ROUTE_OVN_NB_REMOTE"); v != "" {
		cfg.OVNNBRemote = v
	}
	if v := os.Getenv("OVN_ROUTE_BRIDGE_DEV"); v != "" {
		cfg.BridgeDev = v
	}
	if v := os.Getenv("OVN_ROUTE_VRF_NAME"); v != "" {
		cfg.VRFName = v
	}
	if v := os.Getenv("OVN_ROUTE_VETH_NEXTHOP"); v != "" {
		cfg.VethNexthop = v
	}
	if v := os.Getenv("OVN_ROUTE_NETWORK_CIDR"); v != "" {
		cfg.NetworkCIDRs = splitAndTrim(v, ",")
	}
	if v := os.Getenv("OVN_ROUTE_GATEWAY_PORT"); v != "" {
		cfg.GatewayPort = v
	}
	if v := os.Getenv("OVN_ROUTE_OVS_WRAPPER"); v != "" {
		cfg.OVSWrapper = v
	}
	if v := os.Getenv("OVN_ROUTE_ROUTE_TABLE_ID"); v != "" {
		var id int
		if _, err := fmt.Sscanf(v, "%d", &id); err == nil {
			cfg.RouteTableID = id
		}
	}
	if v := os.Getenv("OVN_ROUTE_RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.ReconcileInterval = d
		}
	}
	if v := os.Getenv("OVN_ROUTE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("OVN_ROUTE_DRY_RUN"); v == "1" || v == "true" {
		cfg.DryRun = true
	}
	if v := os.Getenv("OVN_ROUTE_CLEANUP_ON_SHUTDOWN"); v == "0" || v == "false" {
		cfg.CleanupOnShutdown = false
	}
}
