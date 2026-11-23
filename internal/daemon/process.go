package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/itdrills/kubedrill/internal/brand"
)

// ErrNotRunning means no simulator server is running for this home.
var ErrNotRunning = errors.New(brand.Binary + " is not running (start it with `" + brand.Binary + " start`)")

// Probe checks that the server described by si is alive and is really our
// simulator instance (same instance ID), not some other program on the port.
func (h Home) Probe(si *ServerInfo) (*Info, error) {
	c, err := h.HTTPClient(2 * time.Second)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(http.MethodGet, si.URL()+brand.AdminPathPrefix+"/info", nil)
	req.Header.Set("Authorization", "Bearer "+si.Token)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s from %s", resp.Status, si.Address)
	}
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.Product != brand.Name || info.InstanceID != si.InstanceID {
		return nil, fmt.Errorf("the server on %s is not the expected %s instance", si.Address, brand.Name)
	}
	return &info, nil
}

// Running returns the server info if a healthy server is running.
func (h Home) Running() (*ServerInfo, error) {
	si, err := h.ReadServerInfo()
	if err != nil {
		return nil, ErrNotRunning
	}
	if !processAlive(si.PID) {
		_ = os.Remove(h.ServerFile())
		return nil, ErrNotRunning
	}
	if _, err := h.Probe(si); err != nil {
		return nil, fmt.Errorf("%s process %d exists but does not answer: %w", brand.Binary, si.PID, err)
	}
	return si, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// StartBackground launches `<binary> serve <args>` detached from the
// terminal and waits until it is healthy.
func (h Home) StartBackground(serveArgs []string, timeout time.Duration) (*ServerInfo, error) {
	if si, err := h.Running(); err == nil {
		return si, nil
	}
	if err := h.Ensure(); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(h.LogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logf.Close() }() // the child keeps its own descriptor
	cmd := exec.Command(exe, append([]string{"serve"}, serveArgs...)...)
	cmd.Env = append(os.Environ(), brand.HomeEnv+"="+h.Dir)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("server exited during startup (%v); see %s", err, h.LogFile())
		case <-time.After(100 * time.Millisecond):
		}
		si, err := h.ReadServerInfo()
		if err == nil && si.PID == pid {
			if _, err := h.Probe(si); err == nil {
				_ = cmd.Process.Release()
				return si, nil
			}
		}
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("server did not become ready within %s; see %s", timeout, h.LogFile())
}

// StopServer terminates the running server gracefully.
func (h Home) StopServer(timeout time.Duration) (*ServerInfo, error) {
	si, err := h.ReadServerInfo()
	if err != nil || !processAlive(si.PID) {
		_ = os.Remove(h.ServerFile())
		return nil, ErrNotRunning
	}
	if _, err := h.Probe(si); err != nil {
		return nil, fmt.Errorf("refusing to signal pid %d: %w", si.PID, err)
	}
	p, _ := os.FindProcess(si.PID)
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(si.PID) {
			_ = os.Remove(h.ServerFile())
			return si, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = p.Kill()
	_ = os.Remove(h.ServerFile())
	return si, nil
}
