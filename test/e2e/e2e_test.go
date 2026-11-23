//go:build e2e

// Package e2e runs the real kubectl binary against a real kubedrill process.
// Run with: make e2e  (requires kubectl in PATH)
package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

type env struct {
	t        *testing.T
	bin      string
	dir      string
	kubecfg  string
	port     int
	prodHits *atomic.Int64
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func setup(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not found in PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubedrill")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/kubedrill")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// A fake "production" cluster that must never receive a request.
	var hits atomic.Int64
	prod := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "prod must not be contacted", http.StatusTeapot)
	}))
	t.Cleanup(prod.Close)
	kubecfg := filepath.Join(dir, "config")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: prod
contexts:
- context:
    cluster: prod
    user: prod-admin
  name: prod
current-context: prod
users:
- name: prod-admin
  user:
    token: prod-token
`, prod.URL)
	if err := os.WriteFile(kubecfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, bin: bin, dir: dir, kubecfg: kubecfg, port: freePort(t), prodHits: &hits}
	t.Cleanup(func() { _, _ = e.run(e.bin, "stop") })
	return e
}

func (e *env) environ() []string {
	return append(os.Environ(),
		"KUBEDRILL_HOME="+filepath.Join(e.dir, "home"),
		"KUBECONFIG="+e.kubecfg,
		"HOME="+e.dir, // isolates kubectl's discovery cache
		// Deterministic and offline: the registry lookup has its own tests.
		"KUBEDRILL_IMAGE_CHECK=off",
	)
}

func (e *env) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = e.environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) sim(args ...string) string {
	e.t.Helper()
	out, err := e.run(e.bin, args...)
	if err != nil {
		e.t.Fatalf("kubedrill %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) kubectl(args ...string) string {
	e.t.Helper()
	out, err := e.run("kubectl", append([]string{"--context", "kubedrill"}, args...)...)
	if err != nil {
		e.t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) eventually(desc string, timeout time.Duration, fn func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for: %s", desc)
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("output does not contain %q:\n%s", w, out)
		}
	}
}

func TestKubectlEndToEnd(t *testing.T) {
	e := setup(t)
	before, _ := clientcmd.LoadFromFile(e.kubecfg)

	out := e.sim("start", "--port", fmt.Sprint(e.port), "--time-scale", "2")
	mustContain(t, out, "started")

	// Kubeconfig safety: prod untouched, current-context unchanged.
	after, err := clientcmd.LoadFromFile(e.kubecfg)
	if err != nil {
		t.Fatal(err)
	}
	if after.CurrentContext != "prod" || after.Clusters["prod"].Server != before.Clusters["prod"].Server ||
		after.AuthInfos["prod-admin"].Token != "prod-token" {
		t.Fatalf("existing kubeconfig entries changed")
	}

	mustContain(t, e.kubectl("get", "nodes"), "kubedrill-control-plane", "kubedrill-worker2", "Ready")
	mustContain(t, e.kubectl("version"), "Server Version")
	mustContain(t, e.kubectl("api-resources"), "deployments", "replicasets", "configmaps", "events")
	mustContain(t, e.kubectl("explain", "pod.spec.containers.image"), "Container image name")

	mustContain(t, e.kubectl("create", "deployment", "nginx", "--image=nginx", "--replicas=2"), "created")
	e.eventually("deployment available", 30*time.Second, func() bool {
		out, _ := e.run("kubectl", "--context", "kubedrill", "get", "deploy", "nginx", "-o", "jsonpath={.status.availableReplicas}")
		return out == "2"
	})
	mustContain(t, e.kubectl("rollout", "status", "deployment/nginx", "--timeout=30s"), "successfully rolled out")
	pods := e.kubectl("get", "deployments,pods", "-o", "wide")
	mustContain(t, pods, "deployment.apps/nginx", "2/2", "Running", "kubedrill-worker")
	podName := strings.TrimPrefix(strings.Fields(e.kubectl("get", "pods", "-l", "app=nginx", "-o", "name"))[0], "pod/")
	mustContain(t, e.kubectl("describe", "pod", podName), "Controlled By:  ReplicaSet/nginx-", "Successfully assigned", "Started container nginx")
	mustContain(t, e.kubectl("logs", podName), "nginx")
	mustContain(t, e.kubectl("get", "events"), "ScalingReplicaSet", "Pulled")
	mustContain(t, e.kubectl("get", "pods", "-A"), "kube-system", "coredns")
	mustContain(t, e.kubectl("get", "deploy", "nginx", "-o", "yaml"), "kind: Deployment", "deployment.kubernetes.io/revision")
	mustContain(t, e.kubectl("get", "deploy", "nginx", "-o", "json"), `"kind": "Deployment"`)
	mustContain(t, e.kubectl("scale", "deployment", "nginx", "--replicas=3"), "scaled")
	mustContain(t, e.kubectl("rollout", "status", "deployment/nginx", "--timeout=30s"), "successfully rolled out")

	manifest := filepath.Join(e.dir, "app.yaml")
	_ = os.WriteFile(manifest, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  mode: demo
---
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  selector:
    app: nginx
  ports:
  - port: 80
`), 0o600)
	mustContain(t, e.kubectl("apply", "-f", manifest), "configmap/app-config created", "service/nginx created")
	mustContain(t, e.kubectl("apply", "-f", manifest), "unchanged")
	e.eventually("service endpoints", 15*time.Second, func() bool {
		out, _ := e.run("kubectl", "--context", "kubedrill", "get", "endpoints", "nginx")
		return strings.Count(out, ":80") >= 3
	})
	mustContain(t, e.kubectl("delete", "-f", manifest), "deleted")
	mustContain(t, e.kubectl("delete", "deployment", "nginx"), "deleted")

	// Restart keeps state.
	e.kubectl("create", "configmap", "persist-me", "--from-literal=a=b")
	e.sim("stop")
	e.sim("start", "--port", fmt.Sprint(e.port))
	mustContain(t, e.kubectl("get", "configmap", "persist-me"), "persist-me")

	// Wrapper refuses foreign contexts.
	if out, err := e.run(e.bin, "kubectl", "--context", "prod", "get", "pods"); err == nil || !strings.Contains(out, "not allowed") {
		t.Fatalf("wrapper must refuse --context: %v %s", err, out)
	}
	mustContain(t, e.sim("kubectl", "get", "nodes"), "kubedrill-worker")

	if n := e.prodHits.Load(); n != 0 {
		t.Fatalf("the fake production cluster received %d request(s)", n)
	}
}

func TestImagePullBackOffChallenge(t *testing.T) {
	e := setup(t)
	e.sim("start", "--port", fmt.Sprint(e.port), "--time-scale", "2")
	mustContain(t, e.sim("challenge", "list"), "image-pull-backoff", "crashloop-missing-env")
	out := e.sim("challenge", "start", "image-pull-backoff")
	mustContain(t, out, "Challenge started", "Safety: your current kubectl context is \"prod\"")

	e.eventually("ImagePullBackOff visible", 30*time.Second, func() bool {
		out, _ := e.run("kubectl", "--context", "kubedrill", "get", "pods")
		return strings.Contains(out, "ImagePullBackOff") || strings.Contains(out, "ErrImagePull")
	})
	pod := ""
	for _, line := range strings.Split(e.kubectl("get", "pods", "--no-headers"), "\n") {
		if strings.Contains(line, "ImagePull") || strings.Contains(line, "ErrImagePull") {
			pod = strings.Fields(line)[0]
		}
	}
	mustContain(t, e.kubectl("describe", "pod", pod), "Failed to pull image", "nginx:1.27-doesnotexist")

	if out, err := e.run(e.bin, "challenge", "check"); err == nil || !strings.Contains(out, "Not solved yet") {
		t.Fatalf("check must fail before the fix: %v\n%s", err, out)
	}
	mustContain(t, e.sim("hint"), "Hint 1/4")
	mustContain(t, e.sim("hint", "--level", "3"), "Hint 3/4", "jsonpath")

	mustContain(t, e.kubectl("set", "image", "deployment/web", "web=nginx:1.27"), "image updated")
	mustContain(t, e.kubectl("rollout", "status", "deployment/web", "--timeout=40s"), "successfully rolled out")
	mustContain(t, e.sim("challenge", "check"), "Solved!", "[x]")
	mustContain(t, e.sim("challenge", "status"), "solved on your own")

	mustContain(t, e.sim("challenge", "reset"), "reset to its starting point")
	e.eventually("fault back after reset", 20*time.Second, func() bool {
		out, _ := e.run("kubectl", "--context", "kubedrill", "get", "deploy", "web", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
		return out == "nginx:1.27-doesnotexist"
	})
	mustContain(t, e.sim("solve"), "Root cause", "NOT changed")
	mustContain(t, e.sim("challenge", "stop"), "restored")
	if out, _ := e.run("kubectl", "--context", "kubedrill", "get", "deploy"); strings.Contains(out, "web") {
		t.Fatalf("stop did not restore the pre-challenge state:\n%s", out)
	}
	if n := e.prodHits.Load(); n != 0 {
		t.Fatalf("the fake production cluster received %d request(s)", n)
	}
}
