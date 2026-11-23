// Package sim contains the simulated cluster: admission (defaulting and
// validation), deterministic controllers (deployment, replicaset, scheduler,
// kubelet, endpoints, garbage collection) and event recording.
package sim

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/itdrills/kubedrill/internal/registry"
	"github.com/itdrills/kubedrill/internal/store"
)

// State is the simulation-internal state that is not visible as Kubernetes
// objects (container runtimes, counters). It is persisted with the store.
type State struct {
	Seq        uint64               `json:"seq"`
	Pods       map[string]*podRT    `json:"pods"`
	NodeImages map[string][]string  `json:"nodeImages"`
	NextPodIP  map[string]int       `json:"nextPodIP"`
	NextSvcIP  int                  `json:"nextSvcIP"`
	NextPort   int32                `json:"nextNodePort"`
	EventIndex map[string]string    `json:"eventIndex"`
	Scheduled  map[string]time.Time `json:"lastSchedAttempt"`
	Extra      map[string]string    `json:"extra,omitempty"`
}

func newState() *State {
	return &State{Pods: map[string]*podRT{}, NodeImages: map[string][]string{}, NextPodIP: map[string]int{},
		NextSvcIP: 2, NextPort: 30000, EventIndex: map[string]string{}, Scheduled: map[string]time.Time{}, Extra: map[string]string{}}
}

// Cluster is the simulated Kubernetes cluster.
type Cluster struct {
	mu    sync.Mutex
	Store *store.Store
	Clock Clock
	st    *State
	seed  string
	// images verifies image references online; nil means offline catalog only.
	images ImageVerifier
}

// Options configure a new cluster.
type Options struct {
	Clock Clock
	// Seed makes generated names reproducible.
	Seed string
	// ImageVerifier enables real registry lookups for image pulls (nil:
	// offline catalog only, deterministic).
	ImageVerifier ImageVerifier
}

// New creates and bootstraps a cluster.
func New(opts Options) *Cluster {
	if opts.Clock == nil {
		opts.Clock = NewRealClock(1)
	}
	if opts.Seed == "" {
		opts.Seed = "kubedrill"
	}
	c := &Cluster{Store: store.New(), Clock: opts.Clock, st: newState(), seed: opts.Seed, images: opts.ImageVerifier}
	c.mu.Lock()
	c.bootstrap()
	c.reconcile()
	c.mu.Unlock()
	return c
}

func (c *Cluster) next() uint64 {
	c.st.Seq++
	return c.st.Seq
}

func (c *Cluster) suffix(n int) string {
	return randString(hash64(c.seed, fmt.Sprint(c.next())), n)
}

func (c *Cluster) newUID() types.UID {
	a := hash64(c.seed, "uid", fmt.Sprint(c.next()))
	b := hash64(c.seed, "uid2", fmt.Sprint(a))
	return types.UID(fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", uint32(a>>32), uint16(a>>16), uint16(a)&0xfff, (uint16(b>>48)&0x3fff)|0x8000, b&0xffffffffffff))
}

func (c *Cluster) now() metav1.Time { return metav1.NewTime(c.Clock.Now()) }

// ---------------------------------------------------------------------------
// Public, locked entry points used by the API layer and the scenario engine.

// Create admits and stores a new object.
func (c *Cluster) Create(res *registry.Resource, obj runtime.Object, dryRun bool) (runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.create(res, obj, dryRun)
	if err == nil && !dryRun {
		c.reconcile()
		if fresh, gerr := c.get(res, out); gerr == nil {
			out = fresh
		}
	}
	return out, err
}

// Mutate atomically reads an object, applies fn and stores the result.
// subresource is "" or "status"/"scale".
func (c *Cluster) Mutate(res *registry.Resource, ns, name, subresource string, dryRun bool, fn func(cur runtime.Object) (runtime.Object, error)) (runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, err := c.Store.Get(res.GVR, ns, name)
	if err != nil {
		return nil, err
	}
	next, err := fn(cur.DeepCopyObject())
	if err != nil {
		return nil, err
	}
	out, err := c.update(res, cur, next, subresource, dryRun)
	if err == nil && !dryRun {
		c.reconcile()
		if fresh, gerr := c.get(res, out); gerr == nil {
			out = fresh
		}
	}
	return out, err
}

// Update replaces an object (PUT).
func (c *Cluster) Update(res *registry.Resource, obj runtime.Object, subresource string, dryRun bool) (runtime.Object, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	return c.Mutate(res, m.GetNamespace(), m.GetName(), subresource, dryRun, func(cur runtime.Object) (runtime.Object, error) {
		return obj, nil
	})
}

// Delete removes an object, cascading to dependents unless orphaned.
func (c *Cluster) Delete(res *registry.Resource, ns, name string, propagation metav1.DeletionPropagation, dryRun bool) (runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, err := c.Store.Get(res.GVR, ns, name)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return obj, nil
	}
	if res.Kind == "Namespace" && isSystemNamespace(name) {
		return nil, apierrors.NewForbidden(res.GVR.GroupResource(), name, fmt.Errorf("this namespace may not be deleted"))
	}
	if err := c.delete(res, obj, propagation); err != nil {
		return nil, err
	}
	c.reconcile()
	return obj, nil
}

// Tick advances time-based simulation (pull, crash, probes).
func (c *Cluster) Tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcile()
}

// WithLock runs fn while holding the cluster lock; used by the scenario
// engine to apply several changes atomically.
func (c *Cluster) WithLock(fn func(tx *Tx) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := fn(&Tx{c: c})
	c.reconcile()
	return err
}

// Tx gives scenario code transactional access to the cluster.
type Tx struct{ c *Cluster }

// Create creates an object within the transaction.
func (t *Tx) Create(res *registry.Resource, obj runtime.Object) (runtime.Object, error) {
	return t.c.create(res, obj, false)
}

// Update updates an object within the transaction.
func (t *Tx) Update(res *registry.Resource, obj runtime.Object) (runtime.Object, error) {
	m, _ := meta.Accessor(obj)
	cur, err := t.c.Store.Get(res.GVR, m.GetNamespace(), m.GetName())
	if err != nil {
		return nil, err
	}
	m.SetResourceVersion("")
	return t.c.update(res, cur, obj, "", false)
}

// Delete deletes an object within the transaction (background cascade).
func (t *Tx) Delete(res *registry.Resource, ns, name string) error {
	obj, err := t.c.Store.Get(res.GVR, ns, name)
	if err != nil {
		return err
	}
	return t.c.delete(res, obj, metav1.DeletePropagationBackground)
}

// Reconcile runs controllers until stable.
func (t *Tx) Reconcile() { t.c.reconcile() }

// Snapshot captures store and simulation state.
type Snapshot struct {
	Store *store.Snapshot `json:"store"`
	State *State          `json:"state"`
}

// Snapshot returns a consistent copy of the whole cluster.
func (c *Cluster) Snapshot() (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot()
}

func (c *Cluster) snapshot() (*Snapshot, error) {
	s, err := c.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(c.st)
	if err != nil {
		return nil, err
	}
	st := newState()
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	return &Snapshot{Store: s, State: st}, nil
}

// Restore replaces the cluster state.
func (c *Cluster) Restore(s *Snapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restore(s)
}

func (c *Cluster) restore(s *Snapshot) error {
	if s == nil || s.Store == nil {
		return fmt.Errorf("empty snapshot")
	}
	if err := c.Store.Restore(s.Store); err != nil {
		return err
	}
	st := newState()
	if s.State != nil {
		b, _ := json.Marshal(s.State)
		_ = json.Unmarshal(b, st)
	}
	if st.Pods == nil {
		st.Pods = map[string]*podRT{}
	}
	c.st = st
	c.ensureIngressAddon()
	c.reconcile()
	return nil
}

// Reset restores the pristine bootstrap state.
func (c *Cluster) Reset() error {
	fresh := New(Options{Clock: c.Clock, Seed: c.seed})
	snap, err := fresh.Snapshot()
	if err != nil {
		return err
	}
	return c.Restore(snap)
}

// GetExtra returns small opaque values (e.g. active challenge) in the
// persisted simulation state.
func (c *Cluster) GetExtra(k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Extra[k]
}

// SetExtra sets an opaque persisted value; empty deletes it.
func (c *Cluster) SetExtra(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.Extra == nil {
		c.st.Extra = map[string]string{}
	}
	if v == "" {
		delete(c.st.Extra, k)
	} else {
		c.st.Extra[k] = v
	}
}

// ---------------------------------------------------------------------------
// Internal helpers (lock held).

func (c *Cluster) get(res *registry.Resource, obj runtime.Object) (runtime.Object, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	return c.Store.Get(res.GVR, m.GetNamespace(), m.GetName())
}

func (c *Cluster) create(res *registry.Resource, obj runtime.Object, dryRun bool) (runtime.Object, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if res.Namespaced {
		if m.GetNamespace() == "" {
			m.SetNamespace("default")
		}
		if _, err := c.Store.Get(corev1.SchemeGroupVersion.WithResource("namespaces"), "", m.GetNamespace()); err != nil {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, m.GetNamespace())
		}
	} else {
		m.SetNamespace("")
	}
	if m.GetName() == "" && m.GetGenerateName() != "" {
		m.SetName(m.GetGenerateName() + c.suffix(5))
	}
	m.SetUID(c.newUID())
	m.SetCreationTimestamp(c.now())
	m.SetResourceVersion("")
	m.SetDeletionTimestamp(nil)
	m.SetManagedFields(nil)
	if hasGeneration(res) {
		m.SetGeneration(1)
	}
	if ing, ok := obj.(*networkingv1.Ingress); ok {
		c.defaultIngressClass(ing)
	}
	c.setDefaults(res, obj)
	if err := c.validate(res, obj, nil); err != nil {
		return nil, err
	}
	if dryRun {
		return obj, nil
	}
	if _, err := c.Store.Get(res.GVR, m.GetNamespace(), m.GetName()); err == nil {
		return nil, apierrors.NewAlreadyExists(res.GVR.GroupResource(), m.GetName())
	}
	c.allocate(res, obj)
	return c.Store.Create(res.GVR, obj)
}

func hasGeneration(res *registry.Resource) bool {
	switch res.Kind {
	case "Deployment", "ReplicaSet", "Pod", "Service", "Namespace", "Node", "Ingress", "IngressClass":
		return true
	}
	return false
}

func (c *Cluster) update(res *registry.Resource, cur, next runtime.Object, subresource string, dryRun bool) (runtime.Object, error) {
	cm, _ := meta.Accessor(cur)
	nm, err := meta.Accessor(next)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if nm.GetName() != "" && nm.GetName() != cm.GetName() {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", nm.GetName(), cm.GetName()))
	}
	if rv := nm.GetResourceVersion(); rv != "" && rv != cm.GetResourceVersion() {
		return nil, apierrors.NewConflict(res.GVR.GroupResource(), cm.GetName(),
			fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	}
	// System-managed metadata is preserved.
	nm.SetName(cm.GetName())
	nm.SetNamespace(cm.GetNamespace())
	nm.SetUID(cm.GetUID())
	nm.SetCreationTimestamp(cm.GetCreationTimestamp())
	nm.SetGeneration(cm.GetGeneration())
	nm.SetResourceVersion(cm.GetResourceVersion())
	nm.SetManagedFields(nil)
	if subresource == "status" {
		next = mergeStatus(cur, next)
	} else {
		next = keepStatus(cur, next)
		c.setDefaults(res, next)
		if err := c.validate(res, next, cur); err != nil {
			return nil, err
		}
		if hasGeneration(res) && specChanged(cur, next) {
			nm2, _ := meta.Accessor(next)
			nm2.SetGeneration(cm.GetGeneration() + 1)
		}
	}
	if dryRun {
		return next, nil
	}
	return c.Store.Update(res.GVR, next)
}

// keepStatus copies the status of cur into next (main resource updates do
// not change status, like the real API server).
func keepStatus(cur, next runtime.Object) runtime.Object {
	cu, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(cur)
	nu, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(next)
	if st, ok := cu["status"]; ok {
		nu["status"] = st
	} else {
		delete(nu, "status")
	}
	out := next.DeepCopyObject()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(nu, out); err != nil {
		return next
	}
	return out
}

// mergeStatus takes only the status of next.
func mergeStatus(cur, next runtime.Object) runtime.Object {
	cu, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(cur)
	nu, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(next)
	if st, ok := nu["status"]; ok {
		cu["status"] = st
	}
	out := cur.DeepCopyObject()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(cu, out); err != nil {
		return cur
	}
	m, _ := meta.Accessor(out)
	nm, _ := meta.Accessor(next)
	m.SetResourceVersion(nm.GetResourceVersion())
	return out
}

func specChanged(a, b runtime.Object) bool {
	au, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(a)
	bu, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(b)
	as, _ := json.Marshal(au["spec"])
	bs, _ := json.Marshal(bu["spec"])
	return string(as) != string(bs)
}

func (c *Cluster) delete(res *registry.Resource, obj runtime.Object, propagation metav1.DeletionPropagation) error {
	m, _ := meta.Accessor(obj)
	if res.Kind == "Namespace" {
		for _, r := range registry.All() {
			if !r.Namespaced {
				continue
			}
			objs, _ := c.Store.List(r.GVR, m.GetName())
			for _, o := range objs {
				om, _ := meta.Accessor(o)
				_, _ = c.Store.Delete(r.GVR, om.GetNamespace(), om.GetName())
				if r.Kind == "Pod" {
					delete(c.st.Pods, string(om.GetUID()))
				}
			}
		}
	}
	if _, err := c.Store.Delete(res.GVR, m.GetNamespace(), m.GetName()); err != nil {
		return err
	}
	if res.Kind == "Pod" {
		if pod, ok := obj.(*corev1.Pod); ok {
			c.podDeleted(pod)
		}
	}
	if propagation == metav1.DeletePropagationOrphan {
		c.orphanDependents(m.GetUID(), m.GetNamespace())
		return nil
	}
	c.collectGarbage(m.GetUID(), m.GetNamespace())
	return nil
}

// collectGarbage deletes objects owned by uid (recursively).
func (c *Cluster) collectGarbage(uid types.UID, ns string) {
	for _, r := range registry.All() {
		if !r.Namespaced {
			continue
		}
		objs, _ := c.Store.List(r.GVR, ns)
		for _, o := range objs {
			om, _ := meta.Accessor(o)
			for _, ref := range om.GetOwnerReferences() {
				if ref.UID == uid {
					_ = c.delete(r, o, metav1.DeletePropagationBackground)
					break
				}
			}
		}
	}
}

func (c *Cluster) orphanDependents(uid types.UID, ns string) {
	for _, r := range registry.All() {
		if !r.Namespaced {
			continue
		}
		objs, _ := c.Store.List(r.GVR, ns)
		for _, o := range objs {
			om, _ := meta.Accessor(o)
			var keep []metav1.OwnerReference
			changed := false
			for _, ref := range om.GetOwnerReferences() {
				if ref.UID == uid {
					changed = true
					continue
				}
				keep = append(keep, ref)
			}
			if changed {
				om.SetOwnerReferences(keep)
				_, _ = c.Store.Update(r.GVR, o)
			}
		}
	}
}

func isSystemNamespace(n string) bool {
	switch n {
	case "default", "kube-system", "kube-public", "kube-node-lease":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Events

// recordEvent creates or aggregates a core/v1 Event, like the kubelet and
// controllers do. Identical events (same object, reason, message) increase
// the count instead of creating new objects.
func (c *Cluster) recordEvent(obj runtime.Object, kind, eventType, reason, message, component, host string) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	ns := m.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	gvr := corev1.SchemeGroupVersion.WithResource("events")
	idx := string(m.GetUID()) + "|" + reason + "|" + message
	now := c.now()
	if name, ok := c.st.EventIndex[idx]; ok {
		if o, err := c.Store.Get(gvr, ns, name); err == nil {
			ev := o.(*corev1.Event)
			ev.Count++
			ev.LastTimestamp = now
			_, _ = c.Store.Update(gvr, ev)
			return
		}
	}
	apiVersion := "v1"
	if r, ok := registry.LookupKind(schema.GroupKind{Kind: kind, Group: groupOfKind(kind)}); ok {
		apiVersion = r.GVR.GroupVersion().String()
	}
	// Like the real event recorder, names embed a monotonically increasing
	// number so that listing order is chronological.
	name := fmt.Sprintf("%s.%x", m.GetName(), c.Clock.Now().UnixNano()+int64(c.next()))
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: c.newUID(), CreationTimestamp: now},
		InvolvedObject: corev1.ObjectReference{
			Kind: kind, Namespace: m.GetNamespace(), Name: m.GetName(), UID: m.GetUID(),
			APIVersion: apiVersion, ResourceVersion: m.GetResourceVersion(),
		},
		Reason: reason, Message: message, Type: eventType,
		Source:         corev1.EventSource{Component: component, Host: host},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
		ReportingController: component, ReportingInstance: host,
	}
	if ev.ReportingInstance == "" {
		ev.ReportingInstance = component
	}
	if kind == "Pod" {
		// kubectl describe searches events by involvedObject fields.
		ev.InvolvedObject.FieldPath = ""
	}
	if _, err := c.Store.Create(gvr, ev); err == nil {
		c.st.EventIndex[idx] = name
	}
}

func groupOfKind(kind string) string {
	switch kind {
	case "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet":
		return "apps"
	case "EndpointSlice":
		return "discovery.k8s.io"
	case "Ingress", "IngressClass":
		return "networking.k8s.io"
	}
	return ""
}

// listTyped is a small generic helper.
func listTyped[T runtime.Object](c *Cluster, gvr schema.GroupVersionResource, ns string) []T {
	objs, _ := c.Store.List(gvr, ns)
	out := make([]T, 0, len(objs))
	for _, o := range objs {
		if t, ok := o.(T); ok {
			out = append(out, t)
		}
	}
	return out
}

func trimImage(s string) string { return strings.TrimSpace(s) }

// Snapshot returns a snapshot inside a transaction.
func (t *Tx) Snapshot() (*Snapshot, error) { return t.c.snapshot() }

// Restore restores a snapshot inside a transaction.
func (t *Tx) Restore(s *Snapshot) error { return t.c.restore(s) }

// Get reads an object inside a transaction.
func (t *Tx) Get(res *registry.Resource, ns, name string) (runtime.Object, error) {
	return t.c.Store.Get(res.GVR, ns, name)
}

// List lists objects inside a transaction.
func (t *Tx) List(res *registry.Resource, ns string) []runtime.Object {
	objs, _ := t.c.Store.List(res.GVR, ns)
	return objs
}

// Extra returns an opaque persisted value inside a transaction.
func (t *Tx) Extra(k string) string { return t.c.st.Extra[k] }

// SetExtra sets an opaque persisted value inside a transaction.
func (t *Tx) SetExtra(k, v string) {
	if t.c.st.Extra == nil {
		t.c.st.Extra = map[string]string{}
	}
	if v == "" {
		delete(t.c.st.Extra, k)
	} else {
		t.c.st.Extra[k] = v
	}
}
