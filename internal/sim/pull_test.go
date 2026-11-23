package sim

import (
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/itdrills/kubedrill/internal/imagecheck"
)

// fakeVerifier answers lookups from a table; unknown references stay
// Pending until released.
type fakeVerifier struct {
	mu      sync.Mutex
	answers map[string]imagecheck.Result
	asked   map[string]int
}

func (f *fakeVerifier) Lookup(registry, repository, reference string) imagecheck.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := registry + "/" + repository + ":" + reference
	f.asked[k]++
	if r, ok := f.answers[k]; ok {
		return r
	}
	return imagecheck.Result{Status: imagecheck.Pending}
}

func (f *fakeVerifier) set(k string, r imagecheck.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[k] = r
}

func newVerifiedCluster(t *testing.T) (*Cluster, *FakeClock, *fakeVerifier) {
	t.Helper()
	fv := &fakeVerifier{answers: map[string]imagecheck.Result{}, asked: map[string]int{}}
	clk := NewFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clk, Seed: "test", ImageVerifier: fv})
	// The bootstrap image (CoreDNS) exists.
	fv.set("registry.k8s.io/coredns/coredns:v1.12.1", imagecheck.Result{Status: imagecheck.Exists})
	return c, clk, fv
}

func TestRegistryIsAuthoritative(t *testing.T) {
	c, clk, fv := newVerifiedCluster(t)
	// Not in the offline catalog's tag pattern, but the registry knows it.
	fv.set("docker.io/library/nginx:1.99", imagecheck.Result{Status: imagecheck.Exists, Digest: "sha256:feed"})
	// In the catalog's tag pattern, but the registry says the tag is gone.
	fv.set("docker.io/library/nginx:1.10", imagecheck.Result{Status: imagecheck.NotFound})
	fv.set("docker.io/someone/private:1", imagecheck.Result{Status: imagecheck.Denied})
	for name, image := range map[string]string{"real": "nginx:1.99", "gone": "nginx:1.10", "private": "someone/private:1"} {
		if _, err := c.Create(res("deployments"), deployment(name, image, 1), false); err != nil {
			t.Fatal(err)
		}
	}
	advance(c, clk, 20*time.Second)
	real := pods(c, "default", "real-")[0]
	if real.Status.Phase != corev1.PodRunning {
		t.Fatalf("nginx:1.99 should run, got %s/%s", real.Status.Phase, waitingReason(real))
	}
	if id := real.Status.ContainerStatuses[0].ImageID; id != "docker.io/library/nginx@sha256:feed" {
		t.Errorf("imageID should carry the registry digest, got %s", id)
	}
	for _, name := range []string{"gone", "private"} {
		p := pods(c, "default", name+"-")[0]
		if r := waitingReason(p); r != "ImagePullBackOff" && r != "ErrImagePull" {
			t.Fatalf("%s: want pull failure, got %q", name, r)
		}
	}
	msg := pods(c, "default", "private-")[0].Status.ContainerStatuses[0].State.Waiting.Message
	if !strings.Contains(msg, "Back-off") && !strings.Contains(msg, "pull access denied") {
		t.Errorf("unexpected message %q", msg)
	}
}

func TestPendingLookupKeepsPulling(t *testing.T) {
	c, clk, fv := newVerifiedCluster(t)
	if _, err := c.Create(res("deployments"), deployment("slow", "redis:7.4", 1), false); err != nil {
		t.Fatal(err)
	}
	advance(c, clk, 10*time.Second)
	p := pods(c, "default", "slow-")[0]
	if r := waitingReason(p); r != "ContainerCreating" {
		t.Fatalf("pending lookup: want ContainerCreating, got %q", r)
	}
	fv.set("docker.io/library/redis:7.4", imagecheck.Result{Status: imagecheck.Exists})
	advance(c, clk, 3*time.Second)
	if p = pods(c, "default", "slow-")[0]; p.Status.Phase != corev1.PodRunning {
		t.Fatalf("after lookup: want Running, got %s/%s", p.Status.Phase, waitingReason(p))
	}
}

func TestUnreachableRegistryFallsBackToCatalog(t *testing.T) {
	c, clk, fv := newVerifiedCluster(t)
	fv.set("docker.io/library/busybox:1.36", imagecheck.Result{Status: imagecheck.Unreachable, Detail: "dial tcp: no such host"})
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace = "offline", "default"
	pod.Spec.Containers = []corev1.Container{{Name: "main", Image: "busybox:1.36", Command: []string{"sleep", "3600"}}}
	if _, err := c.Create(res("pods"), pod, false); err != nil {
		t.Fatal(err)
	}
	advance(c, clk, 10*time.Second)
	o, _ := c.Store.Get(gvrPods, "default", "offline")
	if p := o.(*corev1.Pod); p.Status.Phase != corev1.PodRunning {
		t.Fatalf("offline fallback: want Running, got %s/%s", p.Status.Phase, waitingReason(p))
	}
	found := false
	for _, ev := range listTyped[*corev1.Event](c, corev1.SchemeGroupVersion.WithResource("events"), "default") {
		if ev.Reason == "RegistryUnreachable" && ev.InvolvedObject.Name == "offline" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing RegistryUnreachable warning event")
	}
}

func TestSimulatedImagesAreNeverLookedUp(t *testing.T) {
	c, clk, fv := newVerifiedCluster(t)
	if _, err := c.Create(res("deployments"), deployment("api", "kubedrill/demo-api:1.4", 1), false); err != nil {
		t.Fatal(err)
	}
	advance(c, clk, 10*time.Second)
	for k := range fv.asked {
		if strings.Contains(k, "kubedrill/") {
			t.Fatalf("simulator-only image was looked up: %s", k)
		}
	}
}
