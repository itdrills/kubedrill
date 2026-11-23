// Package brand centralises every user-visible name so the project can be
// renamed by editing this single file (plus go.mod / packaging metadata).
package brand

const (
	// Name is the human readable product name.
	Name = "KubeDrill"
	// Binary is the CLI binary name.
	Binary = "kubedrill"
	// Claim is the short tagline.
	Claim = "Practice Kubernetes. No Cluster Required."
	// ContextName is the kubeconfig context/cluster/user name created by the tool.
	ContextName = "kubedrill"
	// HomeDirName is the per-user state directory below $HOME.
	HomeDirName = ".kubedrill"
	// HomeEnv overrides the state directory location.
	HomeEnv = "KUBEDRILL_HOME"
	// NodePrefix is used for simulated node names.
	NodePrefix = "kubedrill"
	// AdminPathPrefix is the URL prefix of the simulator's own admin API.
	AdminPathPrefix = "/kubedrill/v1"
	// ScenarioAPIVersion is the apiVersion of scenario documents.
	ScenarioAPIVersion = "itskills.github.io/v1alpha1"
	// DefaultPort is the default loopback port of the simulated API server.
	DefaultPort = 16443
)
