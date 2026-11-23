package sim

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func newTestCluster(t *testing.T) (*Cluster, *FakeClock) {
	t.Helper()
	clk := NewFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	return New(Options{Clock: clk, Seed: "test"}), clk
}

func advance(c *Cluster, clk *FakeClock, d time.Duration) {
	for i := time.Duration(0); i < d; i += time.Second {
		clk.Advance(time.Second)
		c.Tick()
	}
}

func deployment(name, image string, replicas int32) *appsv1.Deployment {
	lbl := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: lbl},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image}}},
			},
		},
	}
}

func pods(c *Cluster, ns, prefix string) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range listTyped[*corev1.Pod](c, gvrPods, ns) {
		if strings.HasPrefix(p.Name, prefix) {
			out = append(out, p)
		}
	}
	return out
}

func getDeploy(t *testing.T, c *Cluster, name string) *appsv1.Deployment {
	o, err := c.Store.Get(gvrDeployments, "default", name)
	if err != nil {
		t.Fatal(err)
	}
	return o.(*appsv1.Deployment)
}

func waitingReason(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

func TestBootstrap(t *testing.T) {
	c, _ := newTestCluster(t)
	nodes := listTyped[*corev1.Node](c, gvrNodes, "")
	if len(nodes) != 3 {
		t.Fatalf("want 3 nodes, got %d", len(nodes))
	}
	if len(pods(c, "kube-system", "coredns")) != 2 {
		t.Fatalf("coredns pods missing")
	}
}

func TestDeploymentBecomesAvailable(t *testing.T) {
	c, clk := newTestCluster(t)
	if _, err := c.Create(res("deployments"), deployment("nginx", "nginx", 3), false); err != nil {
		t.Fatal(err)
	}
	ps := pods(c, "default", "nginx-")
	if len(ps) != 3 {
		t.Fatalf("want 3 pods, got %d", len(ps))
	}
	advance(c, clk, 10*time.Second)
	d := getDeploy(t, c, "nginx")
	if d.Status.AvailableReplicas != 3 || d.Status.UpdatedReplicas != 3 {
		t.Fatalf("deployment not available: %+v", d.Status)
	}
	for _, p := range pods(c, "default", "nginx-") {
		if p.Status.Phase != corev1.PodRunning || !podReady(p) || p.Spec.NodeName == "" || p.Status.PodIP == "" {
			t.Fatalf("pod not running: %s %+v", p.Name, p.Status)
		}
		if p.Spec.NodeName == "kubedrill-control-plane" {
			t.Fatalf("pod scheduled onto tainted control plane")
		}
	}
}

func TestDeterministicNames(t *testing.T) {
	a, _ := newTestCluster(t)
	b, _ := newTestCluster(t)
	for _, c := range []*Cluster{a, b} {
		if _, err := c.Create(res("deployments"), deployment("web", "nginx:1.27", 2), false); err != nil {
			t.Fatal(err)
		}
	}
	pa, pb := pods(a, "default", "web-"), pods(b, "default", "web-")
	for i := range pa {
		if pa[i].Name != pb[i].Name {
			t.Fatalf("names differ: %s vs %s", pa[i].Name, pb[i].Name)
		}
	}
}

func TestImagePullBackOffAndRollingUpdate(t *testing.T) {
	c, clk := newTestCluster(t)
	_, _ = c.Create(res("deployments"), deployment("web", "nginx:1.27", 2), false)
	advance(c, clk, 10*time.Second)
	_, err := c.Mutate(res("deployments"), "default", "web", "", false, func(o runtime.Object) (runtime.Object, error) {
		d := o.(*appsv1.Deployment)
		d.Spec.Template.Spec.Containers[0].Image = "nginx:1.27-doesnotexist"
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	advance(c, clk, 30*time.Second)
	var bad, good int
	for _, p := range pods(c, "default", "web-") {
		switch {
		case podReady(p):
			good++
		case waitingReason(p) == "ImagePullBackOff" || waitingReason(p) == "ErrImagePull":
			bad++
		}
	}
	if good != 2 || bad != 1 {
		t.Fatalf("expected 2 old ready pods and 1 broken new pod, got good=%d bad=%d", good, bad)
	}
	// Fix the image: rollout completes and old pods are replaced.
	_, _ = c.Mutate(res("deployments"), "default", "web", "", false, func(o runtime.Object) (runtime.Object, error) {
		d := o.(*appsv1.Deployment)
		d.Spec.Template.Spec.Containers[0].Image = "nginx:1.27.3"
		return d, nil
	})
	advance(c, clk, 30*time.Second)
	d := getDeploy(t, c, "web")
	if d.Status.UpdatedReplicas != 2 || d.Status.AvailableReplicas != 2 || d.Status.Replicas != 2 {
		t.Fatalf("rollout not complete: %+v", d.Status)
	}
	if d.Annotations[revisionAnnotation] != "3" {
		t.Fatalf("want revision 3, got %q", d.Annotations[revisionAnnotation])
	}
}

func TestCrashLoopMissingEnv(t *testing.T) {
	c, clk := newTestCluster(t)
	_, _ = c.Create(res("deployments"), deployment("api", "kubedrill/demo-api:1.4", 1), false)
	advance(c, clk, 40*time.Second)
	p := pods(c, "default", "api-")[0]
	if p.Status.ContainerStatuses[0].RestartCount < 1 {
		t.Fatalf("expected restarts, got %+v", p.Status.ContainerStatuses[0])
	}
	logs, err := c.Logs("default", p.Name, "", true)
	if err != nil || !strings.Contains(strings.Join(logs, "\n"), "DATABASE_URL") {
		t.Fatalf("expected crash logs mentioning DATABASE_URL, got %v %v", logs, err)
	}
}

func TestPendingInsufficientMemory(t *testing.T) {
	c, clk := newTestCluster(t)
	d := deployment("big", "nginx", 1)
	d.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Gi")}
	_, _ = c.Create(res("deployments"), d, false)
	advance(c, clk, 5*time.Second)
	p := pods(c, "default", "big-")[0]
	if p.Spec.NodeName != "" || p.Status.Phase != corev1.PodPending {
		t.Fatalf("pod should be pending")
	}
	if !strings.Contains(p.Status.Conditions[0].Message, "Insufficient memory") {
		t.Fatalf("unexpected message %q", p.Status.Conditions[0].Message)
	}
}

func TestMissingConfigMapRecovers(t *testing.T) {
	c, clk := newTestCluster(t)
	d := deployment("app", "nginx", 1)
	d.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}}}
	_, _ = c.Create(res("deployments"), d, false)
	advance(c, clk, 10*time.Second)
	if r := waitingReason(pods(c, "default", "app-")[0]); r != "CreateContainerConfigError" {
		t.Fatalf("want CreateContainerConfigError, got %q", r)
	}
	_, _ = c.Create(res("configmaps"), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "default"}, Data: map[string]string{"A": "b"}}, false)
	advance(c, clk, 5*time.Second)
	if !podReady(pods(c, "default", "app-")[0]) {
		t.Fatalf("pod should be ready after creating the configmap")
	}
}

func TestSnapshotRestore(t *testing.T) {
	c, clk := newTestCluster(t)
	snap, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Create(res("deployments"), deployment("tmp", "nginx", 1), false)
	advance(c, clk, 3*time.Second)
	if err := c.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if len(pods(c, "default", "tmp-")) != 0 {
		t.Fatal("restore did not remove pods")
	}
}
