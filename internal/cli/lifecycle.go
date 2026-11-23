package cli

import (
	"errors"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/itdrills/kubedrill/internal/brand"
	"github.com/itdrills/kubedrill/internal/daemon"
	"github.com/itdrills/kubedrill/internal/kubeconfig"
	"github.com/itdrills/kubedrill/internal/sim"
)

func (a *app) startCmd() *cobra.Command {
	var port int
	var fresh, noKubeconfig, setCurrent, foreground, verbose bool
	var timeScale float64
	var scenarioDirs []string
	var imageCheck string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the simulated cluster in the background",
		Long: `Start the simulated cluster in the background.

The simulator listens on 127.0.0.1 only. A standalone kubeconfig is written to
the simulator home, and a "` + brand.ContextName + `" context is added to your kubeconfig
(the file kubectl uses: $KUBECONFIG or ~/.kube/config). Existing contexts and
your current-context are never changed unless you pass --set-current-context.
A backup of your kubeconfig is stored in the simulator home before it is modified.

Image references are verified in their public registry (Docker Hub, ghcr.io,
quay.io, ...): a tag that does not exist ends in ErrImagePull like in a real
cluster. Only anonymous HEAD requests are sent (they do not count towards the
Docker Hub pull limit); results are cached in the simulator home. Offline, the
built-in image catalog is used instead. --image-check=off disables lookups.`,
		Example: `  ` + brand.Binary + ` start
  kubectl --context ` + brand.ContextName + ` get nodes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			serveArgs := []string{"--port", strconv.Itoa(port), "--time-scale", strconv.FormatFloat(timeScale, 'f', -1, 64)}
			if fresh {
				serveArgs = append(serveArgs, "--fresh")
			}
			if verbose {
				serveArgs = append(serveArgs, "--verbose")
			}
			mode, err := daemon.ImageCheckMode(imageCheck)
			if err != nil {
				return err
			}
			serveArgs = append(serveArgs, "--image-check", mode)
			for _, d := range scenarioDirs {
				abs, err := filepath.Abs(d)
				if err != nil {
					return err
				}
				serveArgs = append(serveArgs, "--scenarios", abs)
			}
			if foreground {
				return a.runServe(port, fresh, timeScale, scenarioDirs, verbose, mode, func(si *daemon.ServerInfo) {
					_ = a.writeKubeconfigs(si, noKubeconfig, setCurrent)
				})
			}
			if si, err := a.home.Running(); err == nil {
				a.printf("%s is already running on %s (pid %d).\n", brand.Name, si.URL(), si.PID)
				return a.writeKubeconfigs(si, noKubeconfig, setCurrent)
			}
			si, err := a.home.StartBackground(serveArgs, 15*time.Second)
			if err != nil {
				return err
			}
			a.printf("%s started: simulated Kubernetes %s on %s (pid %d)\n", brand.Name, sim.KubernetesVersion, si.URL(), si.PID)
			if err := a.writeKubeconfigs(si, noKubeconfig, setCurrent); err != nil {
				return err
			}
			a.printf("\nTry:\n  kubectl --context %s get nodes\n  %s challenge list\n", brand.ContextName, brand.Binary)
			if mode == "off" {
				a.printf("\nEverything you see is simulated: no containers run and nothing leaves this machine.\n")
			} else {
				a.printf("\nEverything you see is simulated: no containers run. Image tags are verified in their\n" +
					"public registry (anonymous HEAD requests, cached); disable with --image-check=off.\n")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", brand.DefaultPort, "loopback port of the simulated API server")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "ignore the saved state and start with a pristine cluster")
	cmd.Flags().BoolVar(&noKubeconfig, "no-kubeconfig", false, "do not add the context to your kubeconfig (use `"+brand.Binary+" kubectl` or the standalone kubeconfig)")
	cmd.Flags().BoolVar(&setCurrent, "set-current-context", false, "also switch your current-context to "+brand.ContextName+" (opt-in)")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run in the foreground (Ctrl-C stops)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log every API request to the server log")
	cmd.Flags().Float64Var(&timeScale, "time-scale", 1, "speed of the simulation clock (2 = twice as fast)")
	cmd.Flags().StringSliceVar(&scenarioDirs, "scenarios", nil, "additional directories with scenario YAML files")
	cmd.Flags().StringVar(&imageCheck, "image-check", "", "verify image tags in their registry: auto (default, offline fallback) or off (env KUBEDRILL_IMAGE_CHECK)")
	return cmd
}

// writeKubeconfigs writes the standalone kubeconfig and (unless disabled)
// merges the context into the user's kubeconfig.
func (a *app) writeKubeconfigs(si *daemon.ServerInfo, noMerge, setCurrent bool) error {
	ca, err := a.home.CACert()
	if err != nil {
		return err
	}
	e := kubeconfig.Entry{Server: si.URL(), Token: si.Token, CAData: ca}
	if err := kubeconfig.WriteStandalone(a.home.Kubeconfig(), e); err != nil {
		return fmt.Errorf("writing standalone kubeconfig: %w", err)
	}
	a.printf("Standalone kubeconfig: %s\n", a.home.Kubeconfig())
	if noMerge {
		a.printf("Your kubeconfig was not modified (--no-kubeconfig). Use `%s kubectl ...` or\n  export KUBECONFIG=%s\n", brand.Binary, a.home.Kubeconfig())
		return nil
	}
	st := kubeconfig.Inspect()
	if st.HasContext && st.ContextServer == si.URL() && !setCurrent {
		a.printf("Context %q in %s is up to date (current-context unchanged: %s).\n", brand.ContextName, st.Path, orNone(st.CurrentContext))
		return nil
	}
	if err := a.backupKubeconfig(st.Path); err != nil {
		return fmt.Errorf("backing up %s: %w (nothing was modified)", st.Path, err)
	}
	path, err := kubeconfig.Merge(e, setCurrent)
	if err != nil {
		return err
	}
	after := kubeconfig.Inspect()
	a.printf("Added context %q to %s (current-context: %s).\n", brand.ContextName, path, orNone(after.CurrentContext))
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func (a *app) backupKubeconfig(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := a.home.BackupDir()
	name := filepath.Join(dir, "kubeconfig-"+time.Now().Format("20060102-150405"))
	if err := daemon.WriteFileAtomic(name, data, 0o600); err != nil {
		return err
	}
	// Keep the 10 most recent backups.
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "kubeconfig-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > 10 {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
	return nil
}

func (a *app) serveCmd() *cobra.Command {
	var port int
	var fresh, verbose bool
	var timeScale float64
	var dirs []string
	var imageCheck string
	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run the simulator server in the foreground (used internally by start)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runServe(port, fresh, timeScale, dirs, verbose, imageCheck, nil)
		},
	}
	cmd.Flags().IntVar(&port, "port", brand.DefaultPort, "loopback port")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "ignore saved state")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log requests")
	cmd.Flags().Float64Var(&timeScale, "time-scale", 1, "simulation speed")
	cmd.Flags().StringSliceVar(&dirs, "scenarios", nil, "scenario directories")
	cmd.Flags().StringVar(&imageCheck, "image-check", "", "auto or off")
	return cmd
}

func (a *app) runServe(port int, fresh bool, timeScale float64, dirs []string, verbose bool, imageCheck string, onReady func(*daemon.ServerInfo)) error {
	if si, err := a.home.Running(); err == nil {
		return fmt.Errorf("%s is already running on %s (pid %d)", brand.Name, si.URL(), si.PID)
	}
	ready := make(chan *daemon.ServerInfo, 1)
	go func() {
		if si, ok := <-ready; ok && onReady != nil {
			onReady(si)
		}
	}()
	return daemon.Run(daemon.RunOptions{Home: a.home, Port: port, Fresh: fresh, TimeScale: timeScale,
		ScenarioDirs: dirs, Version: Version, Verbose: verbose, ImageCheck: imageCheck, Ready: ready})
}

func (a *app) stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the simulator (state is saved and restored on the next start)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			si, err := a.home.StopServer(5 * time.Second)
			if errors.Is(err, daemon.ErrNotRunning) {
				a.printf("%s is not running.\n", brand.Name)
				return nil
			}
			if err != nil {
				return err
			}
			a.printf("%s stopped (pid %d). State saved to %s.\n", brand.Name, si.PID, a.home.StateFile())
			a.printf("The %q context stays in your kubeconfig for the next start; `%s kubeconfig remove` deletes it.\n", brand.ContextName, brand.Binary)
			return nil
		},
	}
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the simulator is running and how to reach it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			si, err := a.home.Running()
			if err != nil {
				a.printf("%s: stopped\n", brand.Name)
				if !errors.Is(err, daemon.ErrNotRunning) {
					a.printf("  problem: %v\n", err)
				}
				return nil
			}
			info, err := a.home.Probe(si)
			if err != nil {
				return err
			}
			st := kubeconfig.Inspect()
			a.printf("%s: running\n", brand.Name)
			a.printf("  API server:        %s (simulated Kubernetes %s)\n", si.URL(), info.KubernetesVersion)
			a.printf("  PID:               %d, since %s\n", si.PID, si.StartedAt.Local().Format(time.RFC1123))
			a.printf("  Home:              %s\n", a.home.Dir)
			a.printf("  Standalone config: %s\n", a.home.Kubeconfig())
			if info.ImageCheck != "" {
				a.printf("  Image check:       %s\n", info.ImageCheck)
			}
			a.printf("  Your kubeconfig:   %s (context %q present: %v, current-context: %s)\n", st.Path, brand.ContextName, st.HasContext, orNone(st.CurrentContext))
			var c struct {
				Active string `json:"active"`
			}
			if err := a.call("GET", "/challenges", nil, &c); err == nil && c.Active != "" {
				a.printf("  Active challenge:  %s\n", c.Active)
			}
			a.contextNotice(si)
			return nil
		},
	}
}

func (a *app) resetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Reset the simulated cluster to its pristine state (deletes all your resources)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.call("POST", "/reset", nil, nil); err != nil {
				return err
			}
			a.printf("Simulated cluster reset: all workloads, challenge state and events were removed.\n")
			return nil
		},
	}
}

var forbiddenKubectlFlags = []string{"--context", "--kubeconfig", "--cluster", "--server", "-s", "--user", "--token", "--as"}

func (a *app) kubectlCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "kubectl [args...]",
		Short:              "Run kubectl against the simulator only (forces the simulator kubeconfig and context)",
		Example:            "  " + brand.Binary + " kubectl get pods -A",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				for _, f := range forbiddenKubectlFlags {
					if arg == f || strings.HasPrefix(arg, f+"=") {
						return fmt.Errorf("%s is not allowed here: `%s kubectl` always targets the simulator", f, brand.Binary)
					}
				}
			}
			if _, err := a.server(); err != nil {
				return err
			}
			bin, err := exec.LookPath("kubectl")
			if err != nil {
				return errors.New("kubectl not found in PATH; install it (e.g. `brew install kubectl`)")
			}
			full := append([]string{"--kubeconfig", a.home.Kubeconfig(), "--context", brand.ContextName}, args...)
			c := exec.Command(bin, full...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, a.out, a.err
			if err := c.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					return &exitError{code: ee.ExitCode()}
				}
				return err
			}
			return nil
		},
	}
}

func (a *app) kubeconfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "kubeconfig", Short: "Manage the " + brand.ContextName + " kubeconfig context"}
	var setCurrent bool
	merge := &cobra.Command{
		Use:   "merge",
		Short: "Add/update the " + brand.ContextName + " context in your kubeconfig (other entries untouched)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			si, err := a.server()
			if err != nil {
				return err
			}
			return a.writeKubeconfigs(si, false, setCurrent)
		},
	}
	merge.Flags().BoolVar(&setCurrent, "set-current-context", false, "also switch the current-context (opt-in)")
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Remove the " + brand.ContextName + " context/cluster/user from your kubeconfig",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := kubeconfig.Inspect()
			if err := a.backupKubeconfig(st.Path); err != nil {
				return err
			}
			removed, err := kubeconfig.Remove()
			if err != nil {
				return err
			}
			if removed {
				a.printf("Removed %q entries from %s.\n", brand.ContextName, st.Path)
			} else {
				a.printf("No %q entries found in %s.\n", brand.ContextName, st.Path)
			}
			return nil
		},
	}
	path := &cobra.Command{
		Use:   "path",
		Short: "Print the path of the standalone kubeconfig",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			a.printf("%s\n", a.home.Kubeconfig())
		},
	}
	cmd.AddCommand(merge, remove, path)
	return cmd
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			a.printf("%s %s (simulates Kubernetes %s)\n", brand.Binary, Version, sim.KubernetesVersion)
		},
	}
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check prerequisites and the kubeconfig setup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ok := true
			check := func(good bool, msg, fix string) {
				mark := "ok  "
				if !good {
					mark = "FAIL"
					ok = false
				}
				a.printf("[%s] %s\n", mark, msg)
				if !good && fix != "" {
					a.printf("       -> %s\n", fix)
				}
			}
			bin, err := exec.LookPath("kubectl")
			check(err == nil, "kubectl found in PATH "+bin, "install kubectl, e.g. `brew install kubectl`")
			if err == nil {
				out, _ := exec.Command(bin, "version", "--client").Output()
				a.printf("       %s\n", strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
			}
			si, err := a.home.Running()
			check(err == nil, brand.Name+" server running", "run `"+brand.Binary+" start`")
			st := kubeconfig.Inspect()
			check(st.HasContext, fmt.Sprintf("context %q present in %s", brand.ContextName, st.Path), "run `"+brand.Binary+" kubeconfig merge` or use `"+brand.Binary+" kubectl`")
			if si != nil && st.HasContext {
				check(st.ContextServer == si.URL(), fmt.Sprintf("context %q points to the running simulator (%s)", brand.ContextName, st.ContextServer), "run `"+brand.Binary+" kubeconfig merge`")
			}
			if st.HasContext {
				check(kubeconfig.IsLoopback(st.ContextServer), "context "+brand.ContextName+" points to a loopback address", "the context does not belong to "+brand.Name+"; remove or rename it")
			}
			a.printf("current-context: %s\n", orNone(st.CurrentContext))
			if !ok {
				return &exitError{code: 1}
			}
			return nil
		},
	}
}
