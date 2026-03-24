package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
// effectiveNetworkFilters returns manual config if non-empty, otherwise discovered networks.
// This is the single source of truth for the "manual takes precedence" rule, used by
// both NAT filtering (ovn.go) and veth-leak/prefix-list reconciliation (agent.go).
func effectiveNetworkFilters(manual, discovered []*net.IPNet) []*net.IPNet {
	if len(manual) > 0 {
		return manual
	}
	return discovered
}

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

// PortForwardRule describes a single DNAT port forwarding rule.
type PortForwardRule struct {
	Proto    string `yaml:"proto"`     // "tcp" or "udp"
	Port     int    `yaml:"port"`      // incoming port on VIP (1-65535)
	DestAddr string `yaml:"dest_addr"` // backend IP address
	DestPort int    `yaml:"dest_port"` // backend port (0 = same as Port)
}

// PortForwardVIP describes a VIP with its forwarding rules.
type PortForwardVIP struct {
	VIP        string            `yaml:"vip"`        // anycast VIP address
	ManageVIP  bool              `yaml:"manage_vip"` // agent adds/removes VIP on port_forward_dev
	Masquerade bool              `yaml:"masquerade"` // SNAT forwarded traffic with outgoing interface IP
	Rules      []PortForwardRule `yaml:"rules"`
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
	BridgeIP          string // IP to add to br-ex for ARP resolution (default: 169.254.169.254)
	OVSWrapper        string // e.g. "docker exec openvswitch_vswitchd" — prepended to ovs-vsctl/ovs-ofctl calls
	ReconcileInterval time.Duration
	LogLevel          string
	DryRun            bool
	CleanupOnShutdown bool
	DrainOnShutdown   bool
	DrainTimeout      time.Duration
	FRRPrefixList     string // FRR prefix-list name to manage dynamically (e.g. ANNOUNCED-NETWORKS)

	// Stale chassis cleanup: grace period before removing OVN NB entries
	// from chassis that have disappeared from the SB Chassis table.
	// 0 = disabled.
	StaleChassisGracePeriod time.Duration

	// Veth VRF leak settings
	VethLeakEnabled      bool
	VethProviderIP       string // IP of the veth-provider side (default: computed from VethNexthop + 1)
	VethLeakTableID      int    // Routing table for leak default route (default: 200)
	VethLeakRulePriority int    // Policy rule priority (default: 2000)

	// Port forwarding (DNAT) settings
	PortForwardEnabled bool             // derived: len(PortForwards) > 0
	PortForwardDev     string           // loopback device for VIP addresses (default: "loopback1")
	PortForwardTableID int              // routing table for DNAT return traffic (default: 201)
	PortForwards       []PortForwardVIP // VIP forwarding rules from config
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
	BridgeIP          string        `yaml:"bridge_ip"`
	OVSWrapper        string        `yaml:"ovs_wrapper"`
	ReconcileInterval string        `yaml:"reconcile_interval"`
	LogLevel          string        `yaml:"log_level"`
	DryRun            *bool         `yaml:"dry_run"`
	CleanupOnShutdown *bool         `yaml:"cleanup_on_shutdown"`
	DrainOnShutdown   *bool         `yaml:"drain_on_shutdown"`
	DrainTimeout      string        `yaml:"drain_timeout"`

	FRRPrefixList string `yaml:"frr_prefix_list"`

	StaleChassisGracePeriod string `yaml:"stale_chassis_grace_period"`

	VethLeakEnabled      *bool  `yaml:"veth_leak_enabled"`
	VethProviderIP       string `yaml:"veth_provider_ip"`
	VethLeakTableID      *int   `yaml:"veth_leak_table_id"`
	VethLeakRulePriority *int   `yaml:"veth_leak_rule_priority"`

	PortForwardDev     string           `yaml:"port_forward_dev"`
	PortForwardTableID *int             `yaml:"port_forward_table_id"`
	PortForwards       []PortForwardVIP `yaml:"port_forwards"`
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
		fBridgeIP   = fs.String("bridge-ip", "", "IP to add to bridge device for ARP resolution (default: 169.254.169.254)")
		fOVSWrapper = fs.String("ovs-wrapper", "", "Command prefix for ovs-vsctl/ovs-ofctl (e.g. 'docker exec openvswitch_vswitchd')")
		fInterval   = fs.String("reconcile-interval", "", "Full reconciliation interval (e.g. 60s, 5m)")
		fLogLevel          = fs.String("log-level", "", "Log level (debug, info, warn, error)")
		fDryRun            = fs.Bool("dry-run", false, "Dry-run mode: connect and reconcile but only log what would be done")
		fCleanupOnShutdown = fs.Bool("cleanup-on-shutdown", true, "Remove all managed routes on shutdown (SIGINT/SIGTERM)")
		fDrainOnShutdown   = fs.Bool("drain-on-shutdown", true, "Drain HA gateways before shutdown by lowering Gateway_Chassis priority")
		fDrainTimeout      = fs.String("drain-timeout", "", "Max time to wait for gateway drain (e.g. 60s)")

		fFRRPrefixList        = fs.String("frr-prefix-list", "", "FRR prefix-list name to manage dynamically (default: ANNOUNCED-NETWORKS)")
		fStaleGrace           = fs.String("stale-chassis-grace-period", "", "Grace period before cleaning up entries from missing chassis (e.g. 5m, 0 to disable)")
		fVethLeakEnabled      = fs.Bool("veth-leak-enabled", true, "Enable veth VRF route leaking")
		fVethProviderIP       = fs.String("veth-provider-ip", "", "IP of the veth-provider side (default: veth-nexthop + 1)")
		fVethLeakTableID      = fs.Int("veth-leak-table-id", 200, "Routing table ID for veth leak default route (1-252)")
		fVethLeakRulePriority = fs.Int("veth-leak-rule-priority", 2000, "Policy rule priority for veth leak rules")

		fPortForwardDev     = fs.String("port-forward-dev", "", "Loopback device for VIP addresses in VRF (default: loopback1)")
		fPortForwardTableID = fs.Int("port-forward-table-id", 201, "Routing table ID for DNAT return traffic (1-252)")
	)

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if *showVersion {
		return Config{}, errVersionRequested
	}

	// Layer 0: defaults
	cfg := Config{
		BridgeDev:               "br-ex",
		VRFName:                 "vrf-provider",
		VethNexthop:             "169.254.0.1",
		BridgeIP:                "169.254.169.254",
		ReconcileInterval:       60 * time.Second,
		LogLevel:                "info",
		CleanupOnShutdown:       true,
		DrainOnShutdown:         true,
		DrainTimeout:            60 * time.Second,
		FRRPrefixList:           "ANNOUNCED-NETWORKS",
		StaleChassisGracePeriod: 5 * time.Minute,
		VethLeakEnabled:         true,
		VethLeakTableID:         200,
		VethLeakRulePriority:    2000,
		PortForwardDev:          "loopback1",
		PortForwardTableID:      201,
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
		case "bridge-ip":
			cfg.BridgeIP = *fBridgeIP
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
		case "drain-on-shutdown":
			cfg.DrainOnShutdown = *fDrainOnShutdown
		case "drain-timeout":
			if d, err := time.ParseDuration(*fDrainTimeout); err == nil {
				cfg.DrainTimeout = d
			}
		case "frr-prefix-list":
			cfg.FRRPrefixList = *fFRRPrefixList
		case "stale-chassis-grace-period":
			if d, err := time.ParseDuration(*fStaleGrace); err == nil {
				cfg.StaleChassisGracePeriod = d
			} else {
				slog.Warn("ignoring invalid stale-chassis-grace-period flag value", "value", *fStaleGrace, "error", err)
			}
		case "veth-leak-enabled":
			cfg.VethLeakEnabled = *fVethLeakEnabled
		case "veth-provider-ip":
			cfg.VethProviderIP = *fVethProviderIP
		case "veth-leak-table-id":
			cfg.VethLeakTableID = *fVethLeakTableID
		case "veth-leak-rule-priority":
			cfg.VethLeakRulePriority = *fVethLeakRulePriority
		case "port-forward-dev":
			cfg.PortForwardDev = *fPortForwardDev
		case "port-forward-table-id":
			cfg.PortForwardTableID = *fPortForwardTableID
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

	// FRR prefix-list validation
	if cfg.FRRPrefixList != "" && !isValidIdentifier(cfg.FRRPrefixList) {
		return fmt.Errorf("invalid frr-prefix-list: %q (only alphanumeric, hyphen, underscore, dot allowed)", cfg.FRRPrefixList)
	}

	if cfg.DrainTimeout < 0 {
		return fmt.Errorf("invalid drain-timeout: must be non-negative")
	}
	if cfg.DrainOnShutdown && cfg.DrainTimeout == 0 {
		return fmt.Errorf("drain-on-shutdown requires a positive drain-timeout")
	}

	if cfg.StaleChassisGracePeriod < 0 {
		return fmt.Errorf("invalid stale-chassis-grace-period: must be non-negative")
	}

	// Veth leak validation
	if cfg.VethLeakEnabled {
		if cfg.VethLeakTableID < 1 || cfg.VethLeakTableID > 252 {
			return fmt.Errorf("invalid veth-leak-table-id: %d (must be 1-252)", cfg.VethLeakTableID)
		}
		// RouteTableID 0 means "main table" and cannot conflict with an explicit leak table.
		if cfg.VethLeakTableID == cfg.RouteTableID && cfg.RouteTableID != 0 {
			return fmt.Errorf("veth-leak-table-id (%d) must not equal route-table-id (%d)", cfg.VethLeakTableID, cfg.RouteTableID)
		}
		// Auto-compute VethProviderIP from VethNexthop + 1 if not set.
		if cfg.VethProviderIP == "" {
			nexthopIP := net.ParseIP(cfg.VethNexthop)
			cfg.VethProviderIP = nextIPInSubnet(nexthopIP).String()
		}
		if net.ParseIP(cfg.VethProviderIP) == nil {
			return fmt.Errorf("invalid veth-provider-ip: %q", cfg.VethProviderIP)
		}
	}

	// Port forward validation
	cfg.PortForwardEnabled = len(cfg.PortForwards) > 0
	if cfg.PortForwardEnabled {
		if cfg.PortForwardTableID < 1 || cfg.PortForwardTableID > 252 {
			return fmt.Errorf("invalid port-forward-table-id: %d (must be 1-252)", cfg.PortForwardTableID)
		}
		if cfg.PortForwardTableID == cfg.RouteTableID && cfg.RouteTableID != 0 {
			return fmt.Errorf("port-forward-table-id (%d) must not equal route-table-id (%d)", cfg.PortForwardTableID, cfg.RouteTableID)
		}
		if cfg.PortForwardTableID == cfg.VethLeakTableID {
			return fmt.Errorf("port-forward-table-id (%d) must not equal veth-leak-table-id (%d)", cfg.PortForwardTableID, cfg.VethLeakTableID)
		}
		if !isValidIdentifier(cfg.PortForwardDev) {
			return fmt.Errorf("invalid port-forward-dev: %q", cfg.PortForwardDev)
		}
		if !cfg.VethLeakEnabled {
			return fmt.Errorf("port forwarding requires veth_leak_enabled=true (needs veth pair for return traffic)")
		}
		seenVIPs := make(map[string]bool, len(cfg.PortForwards))
		for i, pf := range cfg.PortForwards {
			vipIP := net.ParseIP(pf.VIP)
			if vipIP == nil {
				return fmt.Errorf("port_forwards[%d]: invalid VIP: %q", i, pf.VIP)
			}
			if vipIP.To4() == nil {
				return fmt.Errorf("port_forwards[%d]: IPv6 VIP not supported: %q (only IPv4)", i, pf.VIP)
			}
			if seenVIPs[pf.VIP] {
				return fmt.Errorf("port_forwards[%d]: duplicate VIP: %q", i, pf.VIP)
			}
			seenVIPs[pf.VIP] = true
			if len(pf.Rules) == 0 {
				return fmt.Errorf("port_forwards[%d] (vip=%s): no rules defined", i, pf.VIP)
			}
			seenRules := make(map[string]bool, len(pf.Rules))
			for j, r := range pf.Rules {
				if r.Proto != "tcp" && r.Proto != "udp" {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid proto %q (must be tcp or udp)", i, j, r.Proto)
				}
				if r.Port < 1 || r.Port > 65535 {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid port %d (must be 1-65535)", i, j, r.Port)
				}
				ruleKey := fmt.Sprintf("%s/%d", r.Proto, r.Port)
				if seenRules[ruleKey] {
					return fmt.Errorf("port_forwards[%d].rules[%d]: duplicate %s port %d on VIP %s", i, j, r.Proto, r.Port, pf.VIP)
				}
				seenRules[ruleKey] = true
				destIP := net.ParseIP(r.DestAddr)
				if destIP == nil {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid dest_addr: %q", i, j, r.DestAddr)
				}
				if destIP.To4() == nil {
					return fmt.Errorf("port_forwards[%d].rules[%d]: IPv6 dest_addr not supported: %q (only IPv4)", i, j, r.DestAddr)
				}
				if r.DestPort < 0 || r.DestPort > 65535 {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid dest_port %d (must be 0-65535)", i, j, r.DestPort)
				}
			}
		}
	}

	return nil
}

// nextIPInSubnet returns the next IP address after the given IP.
// NOTE: wraps around to 0.0.0.0 for 255.255.255.255 — callers must validate.
func nextIPInSubnet(ip net.IP) net.IP {
	ip = ip.To4()
	if ip == nil {
		return nil
	}
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

// isValidIdentifier checks that a string contains only safe characters
// for use in shell commands (alphanumeric, hyphen, underscore, dot).
func isValidIdentifier(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
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
	if fc.BridgeIP != "" {
		cfg.BridgeIP = fc.BridgeIP
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
	if fc.DrainOnShutdown != nil {
		cfg.DrainOnShutdown = *fc.DrainOnShutdown
	}
	if fc.DrainTimeout != "" {
		if d, err := time.ParseDuration(fc.DrainTimeout); err == nil {
			cfg.DrainTimeout = d
		} else {
			slog.Warn("ignoring invalid drain_timeout in config file", "value", fc.DrainTimeout, "error", err)
		}
	}
	if fc.FRRPrefixList != "" {
		cfg.FRRPrefixList = fc.FRRPrefixList
	}
	if fc.StaleChassisGracePeriod != "" {
		if d, err := time.ParseDuration(fc.StaleChassisGracePeriod); err == nil {
			cfg.StaleChassisGracePeriod = d
		} else {
			slog.Warn("ignoring invalid stale_chassis_grace_period in config file", "value", fc.StaleChassisGracePeriod, "error", err)
		}
	}
	if fc.VethLeakEnabled != nil {
		cfg.VethLeakEnabled = *fc.VethLeakEnabled
	}
	if fc.VethProviderIP != "" {
		cfg.VethProviderIP = fc.VethProviderIP
	}
	if fc.VethLeakTableID != nil {
		cfg.VethLeakTableID = *fc.VethLeakTableID
	}
	if fc.VethLeakRulePriority != nil {
		cfg.VethLeakRulePriority = *fc.VethLeakRulePriority
	}
	if fc.PortForwardDev != "" {
		cfg.PortForwardDev = fc.PortForwardDev
	}
	if fc.PortForwardTableID != nil {
		cfg.PortForwardTableID = *fc.PortForwardTableID
	}
	if len(fc.PortForwards) > 0 {
		cfg.PortForwards = fc.PortForwards
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
	if v := os.Getenv("OVN_ROUTE_BRIDGE_IP"); v != "" {
		cfg.BridgeIP = v
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
	if v := os.Getenv("OVN_ROUTE_DRAIN_ON_SHUTDOWN"); v == "0" || v == "false" {
		cfg.DrainOnShutdown = false
	}
	if v := os.Getenv("OVN_ROUTE_DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.DrainTimeout = d
		} else {
			slog.Warn("ignoring invalid OVN_ROUTE_DRAIN_TIMEOUT env var", "value", v, "error", err)
		}
	}
	if v := os.Getenv("OVN_ROUTE_FRR_PREFIX_LIST"); v != "" {
		cfg.FRRPrefixList = v
	}
	if v := os.Getenv("OVN_ROUTE_STALE_CHASSIS_GRACE_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.StaleChassisGracePeriod = d
		} else {
			slog.Warn("ignoring invalid OVN_ROUTE_STALE_CHASSIS_GRACE_PERIOD env var", "value", v, "error", err)
		}
	}
	if v := os.Getenv("OVN_ROUTE_VETH_LEAK_ENABLED"); v == "0" || v == "false" {
		cfg.VethLeakEnabled = false
	}
	if v := os.Getenv("OVN_ROUTE_VETH_PROVIDER_IP"); v != "" {
		cfg.VethProviderIP = v
	}
	if v := os.Getenv("OVN_ROUTE_VETH_LEAK_TABLE_ID"); v != "" {
		var id int
		if _, err := fmt.Sscanf(v, "%d", &id); err == nil {
			cfg.VethLeakTableID = id
		}
	}
	if v := os.Getenv("OVN_ROUTE_VETH_LEAK_RULE_PRIORITY"); v != "" {
		var prio int
		if _, err := fmt.Sscanf(v, "%d", &prio); err == nil {
			cfg.VethLeakRulePriority = prio
		}
	}
	if v := os.Getenv("OVN_ROUTE_PORT_FORWARD_DEV"); v != "" {
		cfg.PortForwardDev = v
	}
	if v := os.Getenv("OVN_ROUTE_PORT_FORWARD_TABLE_ID"); v != "" {
		var id int
		if _, err := fmt.Sscanf(v, "%d", &id); err == nil {
			cfg.PortForwardTableID = id
		}
	}
}
