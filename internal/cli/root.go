// Package cli implements the command line interface.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/itdrills/kubedrill/internal/brand"
	"github.com/itdrills/kubedrill/internal/daemon"
	"github.com/itdrills/kubedrill/internal/kubeconfig"
)

// Version is set at build time via -ldflags.
var Version = "dev"

type app struct {
	home daemon.Home
	out  io.Writer
	err  io.Writer
	// inUI is set when the browser UI runs the command; it always targets the
	// simulator, so kubeconfig context hints would only confuse.
	inUI bool
}

// NewRoot builds the command tree.
func NewRoot() *cobra.Command { return newRoot(os.Stdout, os.Stderr, false) }

// newRoot builds the command tree writing to out and errOut (the browser UI
// runs commands in-process with buffers).
func newRoot(out, errOut io.Writer, inUI bool) *cobra.Command {
	a := &app{out: out, err: errOut, inUI: inUI}
	root := &cobra.Command{
		Use:   brand.Binary,
		Short: brand.Name + " – " + brand.Claim,
		Long: brand.Name + " – " + brand.Claim + `

A lightweight, fully local Kubernetes simulator. Use the real kubectl against a
simulated cluster: no containers, no VMs, no real control plane. Practice
troubleshooting with built-in challenges, graded hints and automatic checks.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			h, err := daemon.DefaultHome()
			if err != nil {
				return err
			}
			a.home = h
			return nil
		},
	}
	root.SetOut(a.out)
	root.SetErr(a.err)
	root.AddCommand(a.startCmd(), a.stopCmd(), a.statusCmd(), a.resetCmd(), a.serveCmd(),
		a.kubectlCmd(), a.kubeconfigCmd(), a.challengeCmd(), a.hintCmd(), a.solveCmd(), a.versionCmd(), a.doctorCmd(),
		a.uiCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := NewRoot()
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// printf and eprintf write to the terminal; a failing write to stdout/stderr
// cannot be reported anywhere, so the error is deliberately ignored.
func (a *app) printf(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format, args...) }

func (a *app) eprintf(format string, args ...any) { _, _ = fmt.Fprintf(a.err, format, args...) }

// ---------------------------------------------------------------------------
// Admin API client. It only ever talks to the server recorded in the
// simulator home (loopback), verified by instance ID – never to a cluster
// from the user's kubeconfig.

func (a *app) server() (*daemon.ServerInfo, error) {
	si, err := a.home.Running()
	if err != nil {
		return nil, err
	}
	if !kubeconfig.IsLoopback(si.URL()) {
		return nil, fmt.Errorf("refusing to talk to non-local server %s", si.Address)
	}
	return si, nil
}

func (a *app) call(method, path string, body, out any) error {
	si, err := a.server()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, si.URL()+brand.AdminPathPrefix+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+si.Token)
	req.Header.Set("Content-Type", "application/json")
	client, err := a.home.HTTPClient(30 * time.Second)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e daemon.ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// contextNotice warns when plain `kubectl` would talk to another cluster.
func (a *app) contextNotice(si *daemon.ServerInfo) {
	if a.inUI {
		return
	}
	st := kubeconfig.Inspect()
	switch {
	case !st.HasContext:
		a.eprintf("Note: no %q context in %s. Use `%s kubectl ...` or run `%s kubeconfig merge`.\n", brand.ContextName, st.Path, brand.Binary, brand.Binary)
	case st.ContextServer != si.URL():
		a.eprintf("Warning: context %q points to %s, but the simulator runs on %s. Run `%s kubeconfig merge` to update it.\n", brand.ContextName, st.ContextServer, si.URL(), brand.Binary)
	case st.CurrentContext != brand.ContextName:
		cur := st.CurrentContext
		if cur == "" {
			cur = "<none>"
		}
		a.eprintf("Safety: your current kubectl context is %q. Always add `--context %s` (or use `%s kubectl ...`) so commands reach the simulator, not a real cluster.\n", cur, brand.ContextName, brand.Binary)
	}
}
