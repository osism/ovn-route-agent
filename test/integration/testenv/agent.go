//go:build integration

package testenv

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// AgentConfig captures the config-file fields the integration harness uses
// to run the agent. Zero-valued fields fall back to AgentConfig defaults
// applied by Defaults().
type AgentConfig struct {
	OVNNBRemote   string `yaml:"ovn_nb_remote"`
	OVNSBRemote   string `yaml:"ovn_sb_remote"`
	BridgeDev     string `yaml:"bridge_dev"`
	VRFName       string `yaml:"vrf_name"`
	VethNexthop   string `yaml:"veth_nexthop"`
	FRRPrefixList string `yaml:"frr_prefix_list"`
	LogLevel      string `yaml:"log_level"`

	// Pointers so we can distinguish "unset" from "explicit false".
	DryRun            *bool `yaml:"dry_run,omitempty"`
	CleanupOnShutdown *bool `yaml:"cleanup_on_shutdown,omitempty"`
	DrainOnShutdown   *bool `yaml:"drain_on_shutdown,omitempty"`
	VethLeakEnabled   *bool `yaml:"veth_leak_enabled,omitempty"`

	ReconcileInterval       string `yaml:"reconcile_interval,omitempty"`
	StaleChassisGracePeriod string `yaml:"stale_chassis_grace_period,omitempty"`
	DrainTimeout            string `yaml:"drain_timeout,omitempty"`
}

// Defaults returns an AgentConfig wired for the local test stack:
// TCP NB on 6641, TCP SB on 6642, FRR prefix-list disabled to avoid
// touching FRR config that may not exist.
func Defaults() AgentConfig {
	off := false
	return AgentConfig{
		OVNNBRemote:       "tcp:127.0.0.1:6641",
		OVNSBRemote:       "tcp:127.0.0.1:6642",
		BridgeDev:         DefaultBridgeDev,
		VRFName:           DefaultVRFName,
		VethNexthop:       "169.254.0.1",
		FRRPrefixList:     "",
		LogLevel:          "debug",
		DrainOnShutdown:   &off,
		ReconcileInterval: "5s",
	}
}

// AgentProc is a handle to a running agent subprocess.
type AgentProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	stderr  io.ReadCloser
	configF string

	// readyCh is closed when "agent running" appears on stderr.
	readyCh chan struct{}
	// exitCh receives the process exit error once cmd.Wait() returns.
	exitCh chan error

	mu      sync.Mutex
	logBuf  strings.Builder
	stopped bool
}

// RunAgent writes cfg to a temp YAML file, execs the agent binary, and
// returns a handle. The process is killed on test cleanup if Stop has
// not already been called.
func RunAgent(t *testing.T, cfg AgentConfig) *AgentProc {
	t.Helper()

	configPath := writeTempConfig(t, cfg)

	bin := AgentBinary(t)
	cmd := exec.Command(bin, "--config", configPath)
	cmd.Env = append(os.Environ(), "GOTRACEBACK=all")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Logf("agent started pid=%d binary=%s config=%s", cmd.Process.Pid, bin, configPath)

	p := &AgentProc{
		t:       t,
		cmd:     cmd,
		stderr:  stderr,
		configF: configPath,
		readyCh: make(chan struct{}),
		exitCh:  make(chan error, 1),
	}

	go p.scanStderr()

	go func() {
		p.exitCh <- cmd.Wait()
	}()

	t.Cleanup(func() {
		// Best-effort kill if the test forgot to call Stop. We don't
		// fail the cleanup — that masks the real test failure.
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if !stopped {
			_ = p.cmd.Process.Signal(syscall.SIGKILL)
			<-p.exitCh
		}
	})

	return p
}

// scanStderr forwards agent stderr to t.Log, buffers it for diagnostics,
// and signals readyCh when the "agent running" line appears.
func (p *AgentProc) scanStderr() {
	scanner := bufio.NewScanner(p.stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	readyClosed := false
	for scanner.Scan() {
		line := scanner.Text()
		p.t.Logf("agent: %s", line)
		p.mu.Lock()
		p.logBuf.WriteString(line)
		p.logBuf.WriteByte('\n')
		p.mu.Unlock()
		if !readyClosed && strings.Contains(line, "agent running") {
			close(p.readyCh)
			readyClosed = true
		}
	}
	if !readyClosed {
		// stderr closed before we ever saw "agent running" — unblock
		// any WaitReady caller so they don't hang on a dead process.
		close(p.readyCh)
	}
}

// WaitReady blocks until the agent logs "agent running" (which fires
// after Connect + initial reconcile) or ctx is cancelled.
func (p *AgentProc) WaitReady(ctx context.Context) error {
	select {
	case <-p.readyCh:
		// readyCh may close because the process died early; surface that.
		select {
		case err := <-p.exitCh:
			p.exitCh <- err
			return fmt.Errorf("agent exited before becoming ready: %w (logs: %s)", err, p.LogTail(20))
		default:
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop sends SIGTERM and waits up to timeout for clean exit. If the
// process does not exit in time, it is killed with SIGKILL.
func (p *AgentProc) Stop(timeout time.Duration) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal SIGTERM: %w", err)
	}

	select {
	case err := <-p.exitCh:
		if err != nil {
			// Exit due to signal-induced shutdown is fine; the agent
			// returns nil from Run() when ctx is cancelled, so a
			// graceful SIGTERM yields exit code 0.
			return fmt.Errorf("agent exited with error: %w", err)
		}
		return nil
	case <-time.After(timeout):
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.exitCh
		return fmt.Errorf("agent did not exit within %s, killed", timeout)
	}
}

// LogTail returns the last n lines of agent stderr captured so far.
func (p *AgentProc) LogTail(n int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := strings.Split(strings.TrimRight(p.logBuf.String(), "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// writeTempConfig serialises cfg to a temp YAML file in t.TempDir().
// Pointer fields are emitted only when set, so the agent's own defaults
// apply for everything else.
func writeTempConfig(t *testing.T, cfg AgentConfig) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")

	var b strings.Builder
	w := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %q\n", k, v)
		}
	}
	wb := func(k string, v *bool) {
		if v != nil {
			fmt.Fprintf(&b, "%s: %t\n", k, *v)
		}
	}
	w("ovn_nb_remote", cfg.OVNNBRemote)
	w("ovn_sb_remote", cfg.OVNSBRemote)
	w("bridge_dev", cfg.BridgeDev)
	w("vrf_name", cfg.VRFName)
	w("veth_nexthop", cfg.VethNexthop)
	// frr_prefix_list intentionally allows empty string to disable.
	fmt.Fprintf(&b, "frr_prefix_list: %q\n", cfg.FRRPrefixList)
	w("log_level", cfg.LogLevel)
	w("reconcile_interval", cfg.ReconcileInterval)
	w("stale_chassis_grace_period", cfg.StaleChassisGracePeriod)
	w("drain_timeout", cfg.DrainTimeout)
	wb("dry_run", cfg.DryRun)
	wb("cleanup_on_shutdown", cfg.CleanupOnShutdown)
	wb("drain_on_shutdown", cfg.DrainOnShutdown)
	wb("veth_leak_enabled", cfg.VethLeakEnabled)

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
