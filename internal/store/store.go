// Package store implements the in-memory object store of the simulator:
// typed Kubernetes objects, a global resourceVersion counter, optimistic
// concurrency, watch fan-out with a bounded replay buffer and snapshots for
// persistence and challenge resets.
package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/itdrills/kubedrill/internal/registry"
)

// historySize bounds the watch replay buffer.
const historySize = 2000

// Event is a single change notification.
type Event struct {
	Type   watch.EventType
	GVR    schema.GroupVersionResource
	Object runtime.Object
	RV     uint64
}

type watcher struct {
	gvr schema.GroupVersionResource
	ns  string
	ch  chan Event
}

// Store is a concurrency-safe in-memory object store.
type Store struct {
	mu       sync.RWMutex
	rv       uint64
	objs     map[schema.GroupVersionResource]map[string]runtime.Object
	history  []Event
	watchers map[int]*watcher
	nextW    int
	// OnChange is invoked (outside the lock) after every successful mutation.
	OnChange func()
}

// New returns an empty store.
func New() *Store {
	return &Store{objs: map[schema.GroupVersionResource]map[string]runtime.Object{}, watchers: map[int]*watcher{}}
}

func key(ns, name string) string { return ns + "/" + name }

func gr(gvr schema.GroupVersionResource) schema.GroupResource { return gvr.GroupResource() }

// ResourceVersion returns the current global resource version.
func (s *Store) ResourceVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rv
}

// Get returns a deep copy of the stored object.
func (s *Store) Get(gvr schema.GroupVersionResource, ns, name string) (runtime.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objs[gvr][key(ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(gr(gvr), name)
	}
	return o.DeepCopyObject(), nil
}

// List returns deep copies of all objects of a type in a namespace ("" = all),
// sorted by namespace and name, plus the list resourceVersion.
func (s *Store) List(gvr schema.GroupVersionResource, ns string) ([]runtime.Object, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for k, o := range s.objs[gvr] {
		if ns != "" {
			m, _ := meta.Accessor(o)
			if m.GetNamespace() != ns {
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]runtime.Object, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.objs[gvr][k].DeepCopyObject())
	}
	return out, s.rv
}

// Create stores a new object. The object's resourceVersion is set.
func (s *Store) Create(gvr schema.GroupVersionResource, obj runtime.Object) (runtime.Object, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.objs[gvr] == nil {
		s.objs[gvr] = map[string]runtime.Object{}
	}
	k := key(m.GetNamespace(), m.GetName())
	if _, exists := s.objs[gvr][k]; exists {
		s.mu.Unlock()
		return nil, apierrors.NewAlreadyExists(gr(gvr), m.GetName())
	}
	s.rv++
	m.SetResourceVersion(strconv.FormatUint(s.rv, 10))
	stored := obj.DeepCopyObject()
	s.objs[gvr][k] = stored
	s.emitLocked(Event{Type: watch.Added, GVR: gvr, Object: stored.DeepCopyObject(), RV: s.rv})
	s.mu.Unlock()
	s.changed()
	return obj, nil
}

// Update replaces an existing object. If the object carries a non-empty
// resourceVersion it must match the stored one (optimistic concurrency).
// Updates that do not change anything are no-ops and keep the version.
func (s *Store) Update(gvr schema.GroupVersionResource, obj runtime.Object) (runtime.Object, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	k := key(m.GetNamespace(), m.GetName())
	cur, ok := s.objs[gvr][k]
	if !ok {
		s.mu.Unlock()
		return nil, apierrors.NewNotFound(gr(gvr), m.GetName())
	}
	cm, _ := meta.Accessor(cur)
	if rv := m.GetResourceVersion(); rv != "" && rv != cm.GetResourceVersion() {
		s.mu.Unlock()
		return nil, apierrors.NewConflict(gr(gvr), m.GetName(),
			fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	}
	m.SetResourceVersion(cm.GetResourceVersion())
	if equalJSON(cur, obj) {
		s.mu.Unlock()
		return obj, nil
	}
	s.rv++
	m.SetResourceVersion(strconv.FormatUint(s.rv, 10))
	stored := obj.DeepCopyObject()
	s.objs[gvr][k] = stored
	s.emitLocked(Event{Type: watch.Modified, GVR: gvr, Object: stored.DeepCopyObject(), RV: s.rv})
	s.mu.Unlock()
	s.changed()
	return obj, nil
}

// Delete removes an object and returns its last state.
func (s *Store) Delete(gvr schema.GroupVersionResource, ns, name string) (runtime.Object, error) {
	s.mu.Lock()
	k := key(ns, name)
	cur, ok := s.objs[gvr][k]
	if !ok {
		s.mu.Unlock()
		return nil, apierrors.NewNotFound(gr(gvr), name)
	}
	delete(s.objs[gvr], k)
	s.rv++
	m, _ := meta.Accessor(cur)
	m.SetResourceVersion(strconv.FormatUint(s.rv, 10))
	s.emitLocked(Event{Type: watch.Deleted, GVR: gvr, Object: cur.DeepCopyObject(), RV: s.rv})
	s.mu.Unlock()
	s.changed()
	return cur, nil
}

func (s *Store) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

func (s *Store) emitLocked(e Event) {
	s.history = append(s.history, e)
	if len(s.history) > historySize {
		s.history = s.history[len(s.history)-historySize:]
	}
	for _, w := range s.watchers {
		if w.gvr != e.GVR {
			continue
		}
		if w.ns != "" {
			if m, _ := meta.Accessor(e.Object); m.GetNamespace() != w.ns {
				continue
			}
		}
		select {
		case w.ch <- e:
		default:
			// Slow consumer: drop the watcher; clients re-list and re-watch.
			close(w.ch)
			for id, ww := range s.watchers {
				if ww == w {
					delete(s.watchers, id)
				}
			}
		}
	}
}

// Watch subscribes to changes. If fromRV is 0 the current state is delivered
// first as ADDED events (Kubernetes semantics for an unset resourceVersion).
// Otherwise all buffered events newer than fromRV are replayed; a 410 Gone
// error is returned if fromRV is older than the replay buffer.
func (s *Store) Watch(gvr schema.GroupVersionResource, ns string, fromRV uint64, sendInitial bool) (<-chan Event, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, 1024)
	var initial []Event
	if sendInitial {
		var keys []string
		for k := range s.objs[gvr] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o := s.objs[gvr][k]
			if ns != "" {
				if m, _ := meta.Accessor(o); m.GetNamespace() != ns {
					continue
				}
			}
			initial = append(initial, Event{Type: watch.Added, GVR: gvr, Object: o.DeepCopyObject(), RV: s.rv})
		}
	} else if fromRV < s.rv {
		if len(s.history) == 0 || s.history[0].RV > fromRV+1 {
			if len(s.history) > 0 {
				return nil, nil, apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", fromRV, s.history[0].RV))
			}
		}
		for _, e := range s.history {
			if e.RV > fromRV && e.GVR == gvr {
				if ns != "" {
					if m, _ := meta.Accessor(e.Object); m.GetNamespace() != ns {
						continue
					}
				}
				initial = append(initial, Event{Type: e.Type, GVR: e.GVR, Object: e.Object.DeepCopyObject(), RV: e.RV})
			}
		}
	}
	if len(initial) > cap(ch)-64 {
		ch = make(chan Event, len(initial)+1024)
	}
	for _, e := range initial {
		ch <- e
	}
	id := s.nextW
	s.nextW++
	w := &watcher{gvr: gvr, ns: ns, ch: ch}
	s.watchers[id] = w
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ww, ok := s.watchers[id]; ok && ww == w {
			delete(s.watchers, id)
			close(ch)
		}
	}
	return ch, cancel, nil
}

// Snapshot is the serialisable state of the store.
type Snapshot struct {
	ResourceVersion uint64                       `json:"resourceVersion"`
	Objects         map[string][]json.RawMessage `json:"objects"`
}

func gvrKey(gvr schema.GroupVersionResource) string {
	return gvr.Group + "/" + gvr.Version + "/" + gvr.Resource
}

// Snapshot serialises all objects.
func (s *Store) Snapshot() (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := &Snapshot{ResourceVersion: s.rv, Objects: map[string][]json.RawMessage{}}
	for gvr, m := range s.objs {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, err := json.Marshal(m[k])
			if err != nil {
				return nil, err
			}
			snap.Objects[gvrKey(gvr)] = append(snap.Objects[gvrKey(gvr)], b)
		}
	}
	return snap, nil
}

// Restore replaces the store content with a snapshot. Watchers receive
// DELETED/ADDED/MODIFIED events so that open watches stay consistent.
func (s *Store) Restore(snap *Snapshot) error {
	newObjs := map[schema.GroupVersionResource]map[string]runtime.Object{}
	for _, r := range registry.All() {
		for _, raw := range snap.Objects[gvrKey(r.GVR)] {
			o := r.New()
			if err := json.Unmarshal(raw, o); err != nil {
				return fmt.Errorf("restore %s: %w", r.GVR.Resource, err)
			}
			m, _ := meta.Accessor(o)
			if newObjs[r.GVR] == nil {
				newObjs[r.GVR] = map[string]runtime.Object{}
			}
			newObjs[r.GVR][key(m.GetNamespace(), m.GetName())] = o
		}
	}
	s.mu.Lock()
	if snap.ResourceVersion > s.rv {
		s.rv = snap.ResourceVersion
	}
	// Emit deletions for objects that disappear, then adds/updates.
	for gvr, m := range s.objs {
		for k, o := range m {
			if _, ok := newObjs[gvr][k]; !ok {
				s.rv++
				s.emitLocked(Event{Type: watch.Deleted, GVR: gvr, Object: o, RV: s.rv})
			}
		}
	}
	for gvr, m := range newObjs {
		for k, o := range m {
			old, existed := s.objs[gvr][k]
			if existed && equalJSONIgnoringRV(old, o) {
				om, _ := meta.Accessor(old)
				nm, _ := meta.Accessor(o)
				nm.SetResourceVersion(om.GetResourceVersion())
				continue
			}
			s.rv++
			nm, _ := meta.Accessor(o)
			nm.SetResourceVersion(strconv.FormatUint(s.rv, 10))
			t := watch.Added
			if existed {
				t = watch.Modified
			}
			s.emitLocked(Event{Type: t, GVR: gvr, Object: o.DeepCopyObject(), RV: s.rv})
		}
	}
	s.objs = newObjs
	s.mu.Unlock()
	s.changed()
	return nil
}

func equalJSON(a, b runtime.Object) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func equalJSONIgnoringRV(a, b runtime.Object) bool {
	a = a.DeepCopyObject()
	b = b.DeepCopyObject()
	am, _ := meta.Accessor(a)
	bm, _ := meta.Accessor(b)
	am.SetResourceVersion("")
	bm.SetResourceVersion("")
	return equalJSON(a, b)
}
