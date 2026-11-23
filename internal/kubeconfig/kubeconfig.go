// Package kubeconfig manages the simulator's kubeconfig entries.
//
// Safety rules:
//   - The standalone kubeconfig in the simulator home is always written.
//   - The user's kubeconfig is only modified by adding/updating the entries
//     named brand.ContextName (cluster, user, context). Other entries and the
//     current-context are never changed (unless SetCurrent is explicitly
//     requested via opt-in).
//   - Existing entries with that name that point to a non-loopback server
//     are never overwritten.
package kubeconfig

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/itdrills/kubedrill/internal/brand"
)

// Entry describes how to reach the simulator.
type Entry struct {
	Server string // e.g. https://127.0.0.1:16443
	Token  string
	CAData []byte // PEM CA certificate of the simulator
}

// IsLoopback reports whether a server URL points to the local machine.
func IsLoopback(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func build(e Entry) *clientcmdapi.Config {
	cfg := clientcmdapi.NewConfig()
	add(cfg, e)
	cfg.CurrentContext = brand.ContextName
	return cfg
}

func add(cfg *clientcmdapi.Config, e Entry) {
	cl := clientcmdapi.NewCluster()
	cl.Server = e.Server
	cl.CertificateAuthorityData = e.CAData
	cfg.Clusters[brand.ContextName] = cl
	u := clientcmdapi.NewAuthInfo()
	u.Token = e.Token
	cfg.AuthInfos[brand.ContextName] = u
	ctx := clientcmdapi.NewContext()
	ctx.Cluster = brand.ContextName
	ctx.AuthInfo = brand.ContextName
	ctx.Namespace = "default"
	cfg.Contexts[brand.ContextName] = ctx
}

// WriteStandalone writes a kubeconfig that only knows the simulator.
func WriteStandalone(path string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := clientcmd.WriteToFile(*build(e), path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// ErrForeignContext is returned when an entry with our name already points
// to a non-local server.
type ErrForeignContext struct{ Server string }

func (e *ErrForeignContext) Error() string {
	return fmt.Sprintf("refusing to modify kubeconfig: an existing %q entry points to %s, which is not a local simulator. Rename it or use the standalone kubeconfig", brand.ContextName, e.Server)
}

// pathOptions returns kubectl's own resolution of kubeconfig files
// ($KUBECONFIG list or ~/.kube/config).
func pathOptions() *clientcmd.PathOptions {
	return clientcmd.NewDefaultPathOptions()
}

// UserConfigPath returns the file that would be modified.
func UserConfigPath() string {
	po := pathOptions()
	if f := po.GetEnvVarFiles(); len(f) > 0 {
		return f[0]
	}
	return po.GlobalFile
}

// Merge adds or updates the simulator entries in the user's kubeconfig
// without touching other entries. If setCurrent is true the current-context
// is switched as well (explicit opt-in only).
func Merge(e Entry, setCurrent bool) (string, error) {
	po := pathOptions()
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return "", fmt.Errorf("reading kubeconfig: %w", err)
	}
	if cl, ok := cfg.Clusters[brand.ContextName]; ok && !IsLoopback(cl.Server) {
		return "", &ErrForeignContext{Server: cl.Server}
	}
	if ctx, ok := cfg.Contexts[brand.ContextName]; ok {
		if cl, ok := cfg.Clusters[ctx.Cluster]; ok && !IsLoopback(cl.Server) {
			return "", &ErrForeignContext{Server: cl.Server}
		}
	}
	before := cfg.CurrentContext
	add(cfg, e)
	if setCurrent {
		cfg.CurrentContext = brand.ContextName
	} else {
		cfg.CurrentContext = before
	}
	if err := clientcmd.ModifyConfig(po, *cfg, true); err != nil {
		return "", fmt.Errorf("writing kubeconfig: %w", err)
	}
	return UserConfigPath(), nil
}

// Remove deletes the simulator entries from the user's kubeconfig. If the
// current-context was the simulator it is cleared.
func Remove() (bool, error) {
	po := pathOptions()
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return false, err
	}
	cl, hasCluster := cfg.Clusters[brand.ContextName]
	_, hasCtx := cfg.Contexts[brand.ContextName]
	if !hasCluster && !hasCtx {
		return false, nil
	}
	if hasCluster && !IsLoopback(cl.Server) {
		return false, &ErrForeignContext{Server: cl.Server}
	}
	delete(cfg.Clusters, brand.ContextName)
	delete(cfg.AuthInfos, brand.ContextName)
	delete(cfg.Contexts, brand.ContextName)
	if cfg.CurrentContext == brand.ContextName {
		cfg.CurrentContext = ""
	}
	return true, clientcmd.ModifyConfig(po, *cfg, true)
}

// Status summarises the user's kubeconfig with respect to the simulator.
type Status struct {
	Path           string
	CurrentContext string
	HasContext     bool
	ContextServer  string
}

// Inspect reads the user's kubeconfig without modifying it.
func Inspect() Status {
	st := Status{Path: UserConfigPath()}
	cfg, err := pathOptions().GetStartingConfig()
	if err != nil {
		return st
	}
	st.CurrentContext = cfg.CurrentContext
	if ctx, ok := cfg.Contexts[brand.ContextName]; ok {
		st.HasContext = true
		if cl, ok := cfg.Clusters[ctx.Cluster]; ok {
			st.ContextServer = cl.Server
		}
	}
	return st
}
