package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/itdrills/kubedrill/internal/api"
	"github.com/itdrills/kubedrill/internal/brand"
	"github.com/itdrills/kubedrill/internal/imagecheck"
	"github.com/itdrills/kubedrill/internal/scenario"
	"github.com/itdrills/kubedrill/internal/sim"
)

// RunOptions configure the server process.
type RunOptions struct {
	Home         Home
	Port         int
	Fresh        bool
	TimeScale    float64
	ScenarioDirs []string
	Version      string
	Verbose      bool
	// ImageCheck is "auto" (verify image references in their registry,
	// offline fallback) or "off" (offline catalog only). Empty means
	// $KUBEDRILL_IMAGE_CHECK or "auto".
	ImageCheck string
	// Ready is closed once the server accepts connections (tests).
	Ready chan<- *ServerInfo
	// Stop ends Run when closed (tests); signals are handled otherwise.
	Stop <-chan struct{}
}

// Run executes the simulator server in the foreground until stopped.
func Run(opts RunOptions) error {
	logger := log.New(os.Stderr, brand.Binary+": ", log.LstdFlags)
	if err := opts.Home.Ensure(); err != nil {
		return err
	}
	token, err := opts.Home.LoadOrCreateToken()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	dirs := append([]string{opts.Home.ScenarioDir()}, opts.ScenarioDirs...)
	if env := os.Getenv("KUBEDRILL_SCENARIOS"); env != "" {
		dirs = append(dirs, env)
	}
	catalog, err := scenario.LoadCatalog(dirs...)
	if err != nil {
		return fmt.Errorf("loading built-in scenarios: %w", err)
	}
	for _, w := range catalog.Warnings {
		logger.Printf("scenario warning: %s", w)
	}

	mode, err := ImageCheckMode(opts.ImageCheck)
	if err != nil {
		return err
	}
	simOpts := sim.Options{Clock: sim.NewRealClock(opts.TimeScale)}
	imageCheckInfo := "off"
	if mode == "auto" {
		checker := imagecheck.New(opts.Home.ImageCache())
		if opts.Verbose {
			checker.Logf = logger.Printf
		}
		simOpts.ImageVerifier = checker
		imageCheckInfo = "online"
	}
	cluster := sim.New(simOpts)
	if !opts.Fresh {
		if err := restoreState(cluster, opts.Home.StateFile()); err != nil {
			bad := opts.Home.StateFile() + ".corrupt-" + time.Now().Format("20060102-150405")
			_ = os.Rename(opts.Home.StateFile(), bad)
			logger.Printf("could not restore saved state (%v); started fresh, old state kept at %s", err, bad)
		}
	}

	var dirty atomic.Bool
	cluster.Store.OnChange = func() { dirty.Store(true) }
	var saveMu sync.Mutex
	save := func() {
		saveMu.Lock()
		defer saveMu.Unlock()
		dirty.Store(false)
		snap, err := cluster.Snapshot()
		if err != nil {
			logger.Printf("snapshot: %v", err)
			return
		}
		b, err := json.Marshal(snap)
		if err != nil {
			logger.Printf("snapshot: %v", err)
			return
		}
		if err := WriteFileAtomic(opts.Home.StateFile(), b, 0o600); err != nil {
			logger.Printf("persist: %v", err)
		}
	}

	cert, err := opts.Home.EnsureTLS()
	if err != nil {
		return fmt.Errorf("creating local TLS certificate: %w", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w (is another %s or program using the port? try --port)", addr, err, brand.Binary)
	}
	info := &ServerInfo{PID: os.Getpid(), Address: ln.Addr().String(), Token: token, InstanceID: RandomHex(8),
		StartedAt: time.Now().UTC(), Version: opts.Version}
	engine := &scenario.Engine{Cluster: cluster, Catalog: catalog}
	srv := &api.Server{Cluster: cluster, Token: token, Admin: &admin{
		info:    Info{Product: brand.Name, InstanceID: info.InstanceID, Version: opts.Version, KubernetesVersion: sim.KubernetesVersion, StartedAt: info.StartedAt, ImageCheck: imageCheckInfo},
		cluster: cluster, engine: engine, persist: save,
	}}
	if opts.Verbose {
		srv.Logf = logger.Printf
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ErrorLog:  log.New(io.Discard, "", 0)}
	if err := opts.Home.WriteServerInfo(info); err != nil {
		_ = ln.Close()
		return err
	}
	defer opts.Home.RemoveServerInfo(info.PID)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ServeTLS(ln, "", "") }()
	logger.Printf("%s listening on https://%s (simulated Kubernetes %s)", brand.Name, info.Address, sim.KubernetesVersion)
	if opts.Ready != nil {
		opts.Ready <- info
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastSave := time.Now()
	for {
		select {
		case <-ticker.C:
			cluster.Tick()
			if dirty.Load() && time.Since(lastSave) > 2*time.Second {
				save()
				lastSave = time.Now()
			}
		case sig := <-sigCh:
			logger.Printf("received %s, shutting down", sig)
			return shutdown(httpSrv, save)
		case <-opts.Stop:
			return shutdown(httpSrv, save)
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			save()
			return err
		}
	}
}

func shutdown(srv *http.Server, save func()) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	save()
	return nil
}

func restoreState(c *sim.Cluster, path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snap sim.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return err
	}
	return c.Restore(&snap)
}

// ImageCheckMode resolves the image check setting: the flag value, else
// $KUBEDRILL_IMAGE_CHECK, else "auto".
func ImageCheckMode(flag string) (string, error) {
	v := flag
	if v == "" {
		v = os.Getenv("KUBEDRILL_IMAGE_CHECK")
	}
	switch v {
	case "", "auto", "on", "online":
		return "auto", nil
	case "off", "offline":
		return "off", nil
	}
	return "", fmt.Errorf("invalid image check mode %q (use auto or off)", v)
}
