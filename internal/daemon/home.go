// Package daemon manages the simulator home directory, the background
// server process (start/stop/status) and its runtime (HTTP server, ticking,
// persistence).
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/itdrills/kubedrill/internal/brand"
)

// Home is the per-user state directory.
type Home struct{ Dir string }

// DefaultHome returns $KUBEDRILL_HOME or ~/.kubedrill.
func DefaultHome() (Home, error) {
	if d := os.Getenv(brand.HomeEnv); d != "" {
		return Home{Dir: d}, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return Home{}, err
	}
	return Home{Dir: filepath.Join(h, brand.HomeDirName)}, nil
}

// Kubeconfig and the following methods return paths inside the home directory.
func (h Home) Kubeconfig() string  { return filepath.Join(h.Dir, "kubeconfig") }
func (h Home) ServerFile() string  { return filepath.Join(h.Dir, "server.json") }
func (h Home) StateFile() string   { return filepath.Join(h.Dir, "state.json") }
func (h Home) LogFile() string     { return filepath.Join(h.Dir, "server.log") }
func (h Home) ScenarioDir() string { return filepath.Join(h.Dir, "scenarios") }
func (h Home) BackupDir() string   { return filepath.Join(h.Dir, "backups") }
func (h Home) ImageCache() string  { return filepath.Join(h.Dir, "image-cache.json") }

// Ensure creates the home directory with private permissions.
func (h Home) Ensure() error {
	return os.MkdirAll(h.Dir, 0o700)
}

// ServerInfo describes a running server; written by the server process.
type ServerInfo struct {
	PID        int       `json:"pid"`
	Address    string    `json:"address"` // host:port
	Token      string    `json:"token"`
	InstanceID string    `json:"instanceID"`
	StartedAt  time.Time `json:"startedAt"`
	Version    string    `json:"version"`
}

// URL returns the base URL of the server.
func (s *ServerInfo) URL() string { return "https://" + s.Address }

// ReadServerInfo loads server.json.
func (h Home) ReadServerInfo() (*ServerInfo, error) {
	b, err := os.ReadFile(h.ServerFile())
	if err != nil {
		return nil, err
	}
	var si ServerInfo
	if err := json.Unmarshal(b, &si); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", h.ServerFile(), err)
	}
	return &si, nil
}

// WriteServerInfo atomically writes server.json (mode 0600: contains the token).
func (h Home) WriteServerInfo(si *ServerInfo) error {
	b, _ := json.MarshalIndent(si, "", "  ")
	return WriteFileAtomic(h.ServerFile(), b, 0o600)
}

// RemoveServerInfo deletes server.json if it belongs to pid.
func (h Home) RemoveServerInfo(pid int) {
	if si, err := h.ReadServerInfo(); err == nil && si.PID == pid {
		_ = os.Remove(h.ServerFile())
	}
}

// LoadOrCreateToken returns the persistent API token.
func (h Home) LoadOrCreateToken() (string, error) {
	p := filepath.Join(h.Dir, "token")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return string(b), nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	tok := RandomHex(24)
	return tok, WriteFileAtomic(p, []byte(tok), 0o600)
}

// RandomHex returns n random bytes hex-encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WriteFileAtomic writes via a temp file and rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	// Cleans up the temp file on every error path; after the rename it no longer exists.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
