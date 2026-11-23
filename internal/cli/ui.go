package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/itdrills/kubedrill/internal/brand"
	"github.com/itdrills/kubedrill/internal/daemon"
	"github.com/itdrills/kubedrill/internal/ui"
)

func (a *app) uiCmd() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the browser UI: terminal, challenge task, hints and check on 127.0.0.1",
		Long: `Open the browser UI for the running simulator.

The page is served on 127.0.0.1 only and protected by a random token that is
part of the printed link. Its command line accepts kubectl commands and the
` + brand.Binary + ` challenge commands – it is not a shell. The UI stops with Ctrl+C;
the simulator keeps running.`,
		Example: "  " + brand.Binary + " start\n  " + brand.Binary + " ui",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.server(); err != nil {
				return fmt.Errorf("%w (start the simulator first: `%s start`)", err, brand.Binary)
			}
			ln, err := ui.Listen(port)
			if err != nil {
				return err
			}
			s := &ui.Server{Token: daemon.RandomHex(32), Port: ln.Addr().(*net.TCPAddr).Port, Backend: &uiBackend{a: a}}
			srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
			link := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", s.Port, s.Token)
			a.printf("%s browser UI: %s\n", brand.Name, link)
			a.printf("The link contains a one-time secret – do not share it. Stop with Ctrl+C.\n")
			if !noOpen {
				openBrowser(link)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			errc := make(chan error, 1)
			go func() { errc <- srv.Serve(ln) }()
			select {
			case <-ctx.Done():
				shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				return srv.Shutdown(shutdown)
			case err := <-errc:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			}
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "port on 127.0.0.1 (0 = any free port)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the link but do not open a browser")
	return cmd
}

// openBrowser opens the link in the default browser; failures are ignored
// because the link is printed anyway.
func openBrowser(link string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", link)
	case "linux":
		c = exec.Command("xdg-open", link)
	default:
		return
	}
	_ = c.Start()
}

// uiBackend runs the browser UI's requests against the local simulator.
type uiBackend struct {
	a  *app
	mu sync.Mutex // in-process CLI runs one command at a time
}

func (b *uiBackend) State() (any, any, error) {
	var c daemon.CatalogResponse
	if err := b.a.call("GET", "/challenges", nil, &c); err != nil {
		return nil, nil, err
	}
	if c.Active == "" {
		return &c, nil, nil
	}
	var ch daemon.ChallengeResponse
	if err := b.a.call("GET", "/challenge", nil, &ch); err != nil {
		return &c, nil, nil //nolint:nilerr // challenge ended between the two calls
	}
	return &c, &ch, nil
}

func (b *uiBackend) Run(ctx context.Context, cmd *ui.Command) (string, int, error) {
	switch cmd.Program {
	case ui.Kubectl:
		return b.kubectl(ctx, cmd.Args)
	case ui.Kubedrill:
		return b.kubedrill(cmd.Args)
	}
	return "", 2, fmt.Errorf("unsupported program %q", cmd.Program)
}

func (b *uiBackend) kubectl(ctx context.Context, args []string) (string, int, error) {
	if _, err := b.a.server(); err != nil {
		return "", 1, err
	}
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		return "", 1, errors.New("kubectl not found in PATH; install it (e.g. `brew install kubectl`)")
	}
	full := append([]string{"--kubeconfig", b.a.home.Kubeconfig(), "--context", brand.ContextName}, args...)
	c := exec.CommandContext(ctx, bin, full...)
	var buf bytes.Buffer
	c.Stdin = nil
	c.Stdout, c.Stderr = &buf, &buf
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return buf.String(), ee.ExitCode(), nil
	}
	return buf.String(), 0, err
}

func (b *uiBackend) kubedrill(args []string) (string, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var buf bytes.Buffer
	root := newRoot(&buf, &buf, true)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return buf.String(), ee.code, nil
		}
		_, _ = fmt.Fprintf(&buf, "Error: %v\n", err)
		return buf.String(), 1, nil
	}
	return buf.String(), 0, nil
}
