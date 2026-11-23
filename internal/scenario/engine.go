package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/itskills/kubedrill/internal/registry"
	"github.com/itskills/kubedrill/internal/sim"
)

const (
	extraChallenge = "challenge"
	extraHistory   = "challengeHistory"
)

// Errors returned by the engine.
var (
	ErrNoChallenge = errors.New("no active challenge: run `kubedrill challenge start [id]` (see `kubedrill challenge list`)")
	ErrActive      = errors.New("a challenge is already active")
)

// Engine runs challenges against a simulated cluster.
type Engine struct {
	mu      sync.Mutex
	Cluster *sim.Cluster
	Catalog *Catalog
}

// HistoryEntry records a finished challenge.
type HistoryEntry struct {
	ID         string    `json:"id"`
	Solved     bool      `json:"solved"`
	SelfSolved bool      `json:"selfSolved"`
	Hints      int       `json:"hints"`
	FinishedAt time.Time `json:"finishedAt"`
}

func metaOf(obj runtime.Object) metav1.Object {
	m, _ := meta.Accessor(obj)
	return m
}

// systemNamespace reports namespaces of the simulated platform (control
// plane, addons). Faults are never injected into their workloads.
func systemNamespace(ns string) bool {
	return strings.HasPrefix(ns, "kube-") || ns == sim.IngressNamespace
}

func (e *Engine) loadState(tx *sim.Tx) (*State, error) {
	raw := tx.Extra(extraChallenge)
	if raw == "" {
		return nil, ErrNoChallenge
	}
	var st State
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("corrupt challenge state: %w", err)
	}
	return &st, nil
}

func (e *Engine) saveState(tx *sim.Tx, st *State) {
	if st == nil {
		tx.SetExtra(extraChallenge, "")
		return
	}
	b, _ := json.Marshal(st)
	tx.SetExtra(extraChallenge, string(b))
}

func snapshotBytes(tx *sim.Tx) ([]byte, error) {
	s, err := tx.Snapshot()
	if err != nil {
		return nil, err
	}
	delete(s.State.Extra, extraChallenge)
	return json.Marshal(s)
}

func restoreBytes(tx *sim.Tx, b []byte) error {
	var s sim.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	history := tx.Extra(extraHistory)
	if err := tx.Restore(&s); err != nil {
		return err
	}
	tx.SetExtra(extraHistory, history)
	return nil
}

// History returns finished challenges.
func (e *Engine) History() []HistoryEntry {
	var h []HistoryEntry
	_ = json.Unmarshal([]byte(e.Cluster.GetExtra(extraHistory)), &h)
	return h
}

// render substitutes target placeholders and re-parses the scenario.
func render(s *Scenario, t TargetInfo) (*Scenario, error) {
	out := placeholderRe.ReplaceAllStringFunc(string(s.raw), func(m string) string {
		switch placeholderRe.FindStringSubmatch(m)[1] {
		case "name":
			return t.Name
		case "namespace":
			return t.Namespace
		default:
			return t.Kind
		}
	})
	return Parse([]byte(out), s.Source)
}

// Active returns the active challenge state, if any.
func (e *Engine) Active() (*State, *Scenario, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var st *State
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		var err error
		st, err = e.loadState(tx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	sc, err := e.rendered(st)
	return st, sc, err
}

func (e *Engine) rendered(st *State) (*Scenario, error) {
	s, ok := e.Catalog.Get(st.ID)
	if !ok {
		return nil, fmt.Errorf("scenario %q of the active challenge is no longer available", st.ID)
	}
	return render(s, st.Target)
}

// pickDefault chooses the first scenario that was not solved yet.
func (e *Engine) pickDefault() string {
	solved := map[string]bool{}
	for _, h := range e.History() {
		if h.Solved {
			solved[h.ID] = true
		}
	}
	list := e.Catalog.List()
	for _, s := range list {
		if !solved[s.Metadata.ID] {
			return s.Metadata.ID
		}
	}
	if len(list) > 0 {
		return list[0].Metadata.ID
	}
	return ""
}

// Start begins a challenge: it injects the fault into a matching existing
// object, or creates the scenario's setup objects and injects there.
func (e *Engine) Start(id string) (*State, *Scenario, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id == "" {
		id = e.pickDefault()
	}
	s, ok := e.Catalog.Get(id)
	if !ok {
		return nil, nil, fmt.Errorf("unknown challenge %q (see `kubedrill challenge list`)", id)
	}
	var st *State
	var rendered *Scenario
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		if cur, err := e.loadState(tx); err == nil {
			return fmt.Errorf("%w: %q. Finish it with `kubedrill challenge check` or abandon it with `kubedrill challenge stop`", ErrActive, cur.ID)
		}
		before, err := snapshotBytes(tx)
		if err != nil {
			return err
		}
		target, err := e.chooseOrSetup(tx, s)
		if err != nil {
			_ = restoreBytes(tx, before)
			return err
		}
		rendered, err = render(s, target)
		if err != nil {
			_ = restoreBytes(tx, before)
			return err
		}
		if rendered.Spec.Inject != nil {
			if err := inject(tx, rendered, target); err != nil {
				_ = restoreBytes(tx, before)
				return fmt.Errorf("injecting fault: %w", err)
			}
		}
		tx.Reconcile()
		start, err := snapshotBytes(tx)
		if err != nil {
			return err
		}
		st = &State{ID: s.Metadata.ID, Title: s.Metadata.Title, StartedAt: e.Cluster.Clock.Now(), Target: target, Before: before, Start: start}
		e.saveState(tx, st)
		return nil
	})
	return st, rendered, err
}

func (e *Engine) chooseOrSetup(tx *sim.Tx, s *Scenario) (TargetInfo, error) {
	if t := s.Spec.Target; t != nil {
		r, _ := kindResource(t.Kind)
		sel := labels.Everything()
		if t.LabelSelector != "" {
			sel, _ = labels.Parse(t.LabelSelector)
		}
		var candidates []runtime.Object
		for _, o := range tx.List(r, "") {
			m := metaOf(o)
			if systemNamespace(m.GetNamespace()) || !sel.Matches(labels.Set(m.GetLabels())) {
				continue
			}
			if len(t.Namespaces) > 0 && !contains(t.Namespaces, m.GetNamespace()) {
				continue
			}
			if !suitable(o) {
				continue
			}
			candidates = append(candidates, o)
		}
		if len(candidates) > 0 {
			m := metaOf(candidates[0])
			return TargetInfo{Kind: r.Kind, Name: m.GetName(), Namespace: m.GetNamespace(), Mode: "existing"}, nil
		}
	}
	ns := s.Spec.Namespace
	if ns == "" {
		ns = "default"
	}
	nsRes, _ := kindResource("Namespace")
	var created []ObjectRef
	for _, doc := range registry.SplitYAML([]byte(s.Spec.Setup)) {
		r, obj, err := registry.Decode(doc)
		if err != nil {
			return TargetInfo{}, err
		}
		m := metaOf(obj)
		if r.Namespaced {
			if m.GetNamespace() == "" {
				m.SetNamespace(ns)
			}
			if _, err := tx.Get(nsRes, "", m.GetNamespace()); apierrors.IsNotFound(err) {
				if _, err := tx.Create(nsRes, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: m.GetNamespace()}}); err != nil {
					return TargetInfo{}, err
				}
				tx.Reconcile()
			}
		}
		if _, err := tx.Get(r, m.GetNamespace(), m.GetName()); err == nil {
			if err := tx.Delete(r, m.GetNamespace(), m.GetName()); err != nil {
				return TargetInfo{}, err
			}
		}
		if _, err := tx.Create(r, obj); err != nil {
			return TargetInfo{}, fmt.Errorf("creating %s %s: %w", r.Kind, m.GetName(), err)
		}
		created = append(created, ObjectRef{Kind: r.Kind, Name: m.GetName(), Namespace: m.GetNamespace()})
	}
	tx.Reconcile()
	var ref *ObjectRef
	if s.Spec.Inject != nil && s.Spec.Inject.Object != nil {
		for i := range created {
			if created[i].Kind == s.Spec.Inject.Object.Kind && created[i].Name == s.Spec.Inject.Object.Name {
				ref = &created[i]
			}
		}
	}
	if ref == nil && s.Spec.Target != nil {
		for i := range created {
			if strings.EqualFold(created[i].Kind, s.Spec.Target.Kind) {
				ref = &created[i]
				break
			}
		}
	}
	if ref == nil && len(created) > 0 {
		ref = &created[0]
	}
	if ref == nil {
		return TargetInfo{}, fmt.Errorf("scenario setup created no objects")
	}
	return TargetInfo{Kind: ref.Kind, Name: ref.Name, Namespace: ref.Namespace, Mode: "setup"}, nil
}

// suitable filters candidates whose fault injection would make sense.
func suitable(o runtime.Object) bool {
	if d, ok := o.(*appsv1.Deployment); ok {
		return d.Spec.Replicas != nil && *d.Spec.Replicas > 0 && len(d.Spec.Template.Spec.Containers) > 0
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func inject(tx *sim.Tx, s *Scenario, t TargetInfo) error {
	r, _ := kindResource(t.Kind)
	obj, err := tx.Get(r, t.Namespace, t.Name)
	if err != nil {
		return err
	}
	o := obj.DeepCopyObject()
	o.GetObjectKind().SetGroupVersionKind(r.GVK())
	orig, err := json.Marshal(o)
	if err != nil {
		return err
	}
	pb, _ := json.Marshal(s.Spec.Inject.Patch)
	p, err := jsonpatch.DecodePatch(pb)
	if err != nil {
		return err
	}
	patched, err := p.Apply(orig)
	if err != nil {
		return fmt.Errorf("patch does not apply to %s %s/%s: %w", t.Kind, t.Namespace, t.Name, err)
	}
	_, next, err := registry.Decode(patched)
	if err != nil {
		return err
	}
	_, err = tx.Update(r, next)
	return err
}

// Hint reveals hint level (1-based); level 0 reveals the next one.
func (e *Engine) Hint(level int) (int, int, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var lvl, total int
	var text string
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		st, err := e.loadState(tx)
		if err != nil {
			return err
		}
		sc, err := e.rendered(st)
		if err != nil {
			return err
		}
		total = len(sc.Spec.Hints)
		lvl = level
		if lvl == 0 {
			lvl = min(st.HintsRevealed+1, total)
		}
		if lvl < 1 || lvl > total {
			return fmt.Errorf("hint level must be between 1 and %d", total)
		}
		text = sc.Spec.Hints[lvl-1]
		if lvl > st.HintsRevealed {
			st.HintsRevealed = lvl
			e.saveState(tx, st)
		}
		return nil
	})
	return lvl, total, text, err
}

// Solve reveals the solution. The challenge is not marked as solved.
func (e *Engine) Solve() (*State, *Scenario, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var st *State
	var sc *Scenario
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		var err error
		if st, err = e.loadState(tx); err != nil {
			return err
		}
		if sc, err = e.rendered(st); err != nil {
			return err
		}
		st.SolutionRevealed = true
		e.saveState(tx, st)
		return nil
	})
	return st, sc, err
}

// Check evaluates the success criteria. If record is true and all pass,
// the challenge is marked solved.
func (e *Engine) Check(record bool) (*State, *CheckResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var st *State
	var res *CheckResult
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		var err error
		if st, err = e.loadState(tx); err != nil {
			return err
		}
		sc, err := e.rendered(st)
		if err != nil {
			return err
		}
		res = Evaluate(tx, sc)
		if record {
			st.Checks++
			if res.Passed && !st.Solved {
				now := e.Cluster.Clock.Now()
				st.Solved = true
				st.SolvedAt = &now
				st.SelfSolved = !st.SolutionRevealed
			}
			e.saveState(tx, st)
		}
		return nil
	})
	return st, res, err
}

// Reset restores the broken starting point of the active challenge.
func (e *Engine) Reset() (*State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var st *State
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		var err error
		if st, err = e.loadState(tx); err != nil {
			return err
		}
		if err := restoreBytes(tx, st.Start); err != nil {
			return err
		}
		// A challenge solved once stays solved; reset is for practising again.
		st.Resets++
		e.saveState(tx, st)
		return nil
	})
	return st, err
}

// Stop ends the active challenge. If restore is true the cluster returns to
// the state before the challenge started.
func (e *Engine) Stop(restore bool) (*State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var st *State
	err := e.Cluster.WithLock(func(tx *sim.Tx) error {
		var err error
		if st, err = e.loadState(tx); err != nil {
			return err
		}
		var h []HistoryEntry
		_ = json.Unmarshal([]byte(tx.Extra(extraHistory)), &h)
		h = append(h, HistoryEntry{ID: st.ID, Solved: st.Solved, SelfSolved: st.SelfSolved, Hints: st.HintsRevealed, FinishedAt: e.Cluster.Clock.Now()})
		if restore {
			if err := restoreBytes(tx, st.Before); err != nil {
				return err
			}
		}
		b, _ := json.Marshal(h)
		tx.SetExtra(extraHistory, string(b))
		e.saveState(tx, nil)
		return nil
	})
	return st, err
}

// ---------------------------------------------------------------------------
// Success criteria

// Evaluate checks all criteria of a rendered scenario.
func Evaluate(tx *sim.Tx, s *Scenario) *CheckResult {
	res := &CheckResult{Passed: true}
	defNS := s.Spec.Namespace
	if defNS == "" {
		defNS = "default"
	}
	for _, c := range s.Spec.Success {
		ns := c.Namespace
		if ns == "" {
			ns = defNS
		}
		ok, detail := evalCriterion(tx, c, ns)
		desc := c.Description
		if desc == "" {
			desc = describe(c, ns)
		}
		res.Criteria = append(res.Criteria, CriterionResult{Description: desc, Passed: ok, Detail: detail})
		if !ok {
			res.Passed = false
		}
	}
	return res
}

func describe(c Criterion, ns string) string {
	switch c.Type {
	case "deploymentAvailable":
		return fmt.Sprintf("Deployment %s/%s is fully rolled out and available", ns, c.Name)
	case "podsReady":
		return fmt.Sprintf("Pods matching %q in %s are Ready", c.Selector, ns)
	case "serviceHasEndpoints":
		return fmt.Sprintf("Service %s/%s has ready endpoints", ns, c.Name)
	case "ingressReady":
		return fmt.Sprintf("Ingress %s/%s routes to ready backends", ns, c.Name)
	case "exists":
		return fmt.Sprintf("%s %s/%s exists", c.Kind, ns, c.Name)
	case "absent":
		return fmt.Sprintf("%s %s/%s does not exist", c.Kind, ns, c.Name)
	default:
		return fmt.Sprintf("%s %s/%s field %s", c.Kind, ns, c.Name, c.Path)
	}
}

func evalCriterion(tx *sim.Tx, c Criterion, ns string) (bool, string) {
	minN := c.Min
	if minN == 0 {
		minN = 1
	}
	switch c.Type {
	case "deploymentAvailable":
		r, _ := kindResource("Deployment")
		o, err := tx.Get(r, ns, c.Name)
		if err != nil {
			return false, "deployment not found"
		}
		d := o.(*appsv1.Deployment)
		want := *d.Spec.Replicas
		st := d.Status
		ok := want > 0 && st.ObservedGeneration >= d.Generation && st.UpdatedReplicas == want && st.AvailableReplicas == want && st.Replicas == want
		return ok, fmt.Sprintf("replicas=%d updated=%d available=%d desired=%d", st.Replicas, st.UpdatedReplicas, st.AvailableReplicas, want)
	case "podsReady":
		r, _ := kindResource("Pod")
		sel, err := labels.Parse(c.Selector)
		if err != nil {
			return false, err.Error()
		}
		ready, notReady := 0, 0
		var bad []string
		for _, o := range tx.List(r, ns) {
			p := o.(*corev1.Pod)
			if !sel.Matches(labels.Set(p.Labels)) || p.Status.Phase == corev1.PodSucceeded {
				continue
			}
			if isReady(p) {
				ready++
			} else {
				notReady++
				bad = append(bad, p.Name)
			}
		}
		sort.Strings(bad)
		detail := fmt.Sprintf("%d ready, %d not ready", ready, notReady)
		if len(bad) > 0 {
			detail += " (" + strings.Join(bad, ", ") + ")"
		}
		return ready >= minN && notReady == 0, detail
	case "serviceHasEndpoints":
		r, _ := kindResource("Endpoints")
		o, err := tx.Get(r, ns, c.Name)
		if err != nil {
			return false, "no endpoints object"
		}
		n := 0
		for _, ss := range o.(*corev1.Endpoints).Subsets {
			n += len(ss.Addresses)
		}
		return n >= minN, fmt.Sprintf("%d ready endpoint(s)", n)
	case "ingressReady":
		r, _ := kindResource("Ingress")
		o, err := tx.Get(r, ns, c.Name)
		if err != nil {
			return false, "ingress not found"
		}
		ing := o.(*networkingv1.Ingress)
		get := func(kind, ns, name string) (any, bool) {
			kr, _ := kindResource(kind)
			obj, err := tx.Get(kr, ns, name)
			return obj, err == nil
		}
		if problems := sim.BackendProblems(get, ing); len(problems) > 0 {
			return false, strings.Join(problems, "; ")
		}
		if len(ing.Status.LoadBalancer.Ingress) == 0 {
			return false, "no address assigned by an ingress controller"
		}
		return true, "address " + ing.Status.LoadBalancer.Ingress[0].IP
	case "exists", "absent":
		r, _ := kindResource(c.Kind)
		_, err := tx.Get(r, ns, c.Name)
		if c.Type == "exists" {
			return err == nil, map[bool]string{true: "found", false: "not found"}[err == nil]
		}
		return err != nil, map[bool]string{true: "still exists", false: "not found"}[err == nil]
	case "field":
		r, _ := kindResource(c.Kind)
		o, err := tx.Get(r, ns, c.Name)
		if err != nil {
			return false, "object not found"
		}
		v, found := pointer(o, c.Path)
		switch {
		case c.Equals != nil:
			return found && v == *c.Equals, fmt.Sprintf("value %q", v)
		case c.NotEquals != nil:
			return v != *c.NotEquals, fmt.Sprintf("value %q", v)
		default:
			re, err := regexp.Compile(c.Matches)
			if err != nil {
				return false, err.Error()
			}
			return found && re.MatchString(v), fmt.Sprintf("value %q", v)
		}
	}
	return false, "unknown criterion"
}

func isReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// pointer resolves a JSON pointer on an object and returns it as string.
func pointer(o runtime.Object, path string) (string, bool) {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return "", false
	}
	var cur any = u
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch x := cur.(type) {
		case map[string]any:
			v, ok := x[seg]
			if !ok {
				return "", false
			}
			cur = v
		case []any:
			var i int
			if _, err := fmt.Sscanf(seg, "%d", &i); err != nil || i < 0 || i >= len(x) {
				return "", false
			}
			cur = x[i]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case nil:
		return "", false
	default:
		b, _ := json.Marshal(v)
		return string(b), true
	}
}
