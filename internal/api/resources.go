package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"github.com/itdrills/kubedrill/internal/registry"
)

// request is a parsed resource request.
type request struct {
	res         *registry.Resource
	namespace   string
	name        string
	subresource string
}

func (s *Server) parse(gv schema.GroupVersion, rest string) (*request, bool, error) {
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) == 1 && segs[0] == "" {
		return nil, true, nil // discovery
	}
	lookup := func(name string) *registry.Resource {
		r, _ := registry.Lookup(gv.WithResource(name))
		return r
	}
	req := &request{}
	if segs[0] == "namespaces" && len(segs) >= 3 && lookup(segs[2]) != nil && segs[2] != "status" && segs[2] != "finalize" {
		req.namespace = segs[1]
		segs = segs[2:]
	}
	req.res = lookup(segs[0])
	if req.res == nil {
		return nil, false, notFoundPath()
	}
	if len(segs) > 1 {
		req.name = segs[1]
	}
	if len(segs) > 2 {
		req.subresource = segs[2]
	}
	if len(segs) > 3 {
		return nil, false, notFoundPath()
	}
	if req.res.Namespaced && req.namespace == "" && req.name != "" {
		return nil, false, notFoundPath()
	}
	if !req.res.Namespaced && req.namespace != "" {
		return nil, false, notFoundPath()
	}
	if req.subresource != "" {
		ok := false
		for _, sr := range req.res.Subresources {
			if sr == req.subresource {
				ok = true
			}
		}
		if !ok {
			return nil, false, notFoundPath()
		}
	}
	return req, false, nil
}

func (s *Server) serveResource(w http.ResponseWriter, r *http.Request, gv schema.GroupVersion, rest string, known bool) {
	if !known || registry.ForGroupVersion(gv) == nil {
		writeStatus(w, notFoundPath())
		return
	}
	req, discovery, err := s.parse(gv, rest)
	if err != nil {
		writeStatus(w, err)
		return
	}
	if discovery {
		writeJSON(w, http.StatusOK, resourceList(gv))
		return
	}
	q := r.URL.Query()
	dryRun := len(q["dryRun"]) > 0
	switch req.subresource {
	case "log":
		s.serveLogs(w, r, req)
		return
	case "exec", "portforward", "attach":
		writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf(
			"%s is not available in the simulator: containers are simulated and no processes run. "+
				"Use 'kubectl logs', 'kubectl describe' and 'kubectl get events' to inspect workloads.", req.subresource)))
		return
	case "scale":
		s.serveScale(w, r, req, dryRun)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if req.name == "" {
			if q.Get("watch") == "true" || q.Get("watch") == "1" {
				s.serveWatch(w, r, req)
				return
			}
			s.serveList(w, r, req)
			return
		}
		obj, err := s.Cluster.Store.Get(req.res.GVR, req.namespace, req.name)
		if err != nil {
			writeStatus(w, err)
			return
		}
		if wantsTable(r) {
			writeJSON(w, http.StatusOK, s.toTable(req.res, []runtime.Object{obj}, r, true, ""))
			return
		}
		writeJSON(w, http.StatusOK, encodeObject(req.res, obj))
	case http.MethodPost:
		if req.name != "" {
			writeStatus(w, apierrors.NewMethodNotSupported(req.res.GVR.GroupResource(), "create on item"))
			return
		}
		obj, warnings, err := decodeBody(r, req.res, nil)
		if err != nil {
			writeStatus(w, err)
			return
		}
		setWarnings(w, warnings)
		m, _ := meta.Accessor(obj)
		if req.res.Namespaced {
			if m.GetNamespace() != "" && m.GetNamespace() != req.namespace {
				writeStatus(w, apierrors.NewBadRequest("the namespace of the provided object does not match the namespace sent on the request"))
				return
			}
			m.SetNamespace(req.namespace)
		}
		out, err := s.Cluster.Create(req.res, obj, dryRun)
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, encodeObject(req.res, out))
	case http.MethodPut:
		obj, warnings, err := decodeBody(r, req.res, nil)
		if err != nil {
			writeStatus(w, err)
			return
		}
		setWarnings(w, warnings)
		m, _ := meta.Accessor(obj)
		if m.GetName() != "" && m.GetName() != req.name {
			writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", m.GetName(), req.name)))
			return
		}
		m.SetName(req.name)
		m.SetNamespace(req.namespace)
		out, err := s.Cluster.Update(req.res, obj, req.subresource, dryRun)
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeJSON(w, http.StatusOK, encodeObject(req.res, out))
	case http.MethodPatch:
		s.servePatch(w, r, req, dryRun)
	case http.MethodDelete:
		if req.name == "" {
			s.serveDeleteCollection(w, r, req, dryRun)
			return
		}
		prop := deletePropagation(r)
		out, err := s.Cluster.Delete(req.res, req.namespace, req.name, prop, dryRun)
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeJSON(w, http.StatusOK, encodeObject(req.res, out))
	default:
		writeStatus(w, apierrors.NewMethodNotSupported(req.res.GVR.GroupResource(), r.Method))
	}
}

func setWarnings(w http.ResponseWriter, warnings []string) {
	for _, msg := range warnings {
		w.Header().Add("Warning", fmt.Sprintf("299 - %q", msg))
	}
}

func deletePropagation(r *http.Request) metav1.DeletionPropagation {
	prop := metav1.DeletePropagationBackground
	if p := r.URL.Query().Get("propagationPolicy"); p != "" {
		prop = metav1.DeletionPropagation(p)
	}
	if r.Body != nil {
		var opts metav1.DeleteOptions
		if b, _ := io.ReadAll(r.Body); len(b) > 0 && json.Unmarshal(b, &opts) == nil {
			if opts.PropagationPolicy != nil {
				prop = *opts.PropagationPolicy
			} else if opts.OrphanDependents != nil && *opts.OrphanDependents {
				prop = metav1.DeletePropagationOrphan
			}
		}
	}
	return prop
}

// decodeBody decodes a request body into a typed object, honouring the
// fieldValidation query parameter (Strict is kubectl's default).
func decodeBody(r *http.Request, res *registry.Resource, data []byte) (runtime.Object, []string, error) {
	if data == nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, nil, apierrors.NewRequestEntityTooLargeError(err.Error())
		}
		data = b
	}
	if ct := r.Header.Get("Content-Type"); strings.Contains(ct, "yaml") {
		j, err := yaml.YAMLToJSON(data)
		if err != nil {
			return nil, nil, apierrors.NewBadRequest(err.Error())
		}
		data = j
	}
	if strings.Contains(r.Header.Get("Content-Type"), "protobuf") {
		return decodeProtobuf(res, data)
	}
	return decodeJSON(r.URL.Query().Get("fieldValidation"), res, data)
}

func decodeJSON(validation string, res *registry.Resource, data []byte) (runtime.Object, []string, error) {
	obj := res.New()
	var tm metav1.TypeMeta
	_ = json.Unmarshal(data, &tm)
	cannot := func(msg string) error {
		return apierrors.NewBadRequest(fmt.Sprintf("%s in version %q cannot be handled as a %s: %s", res.Kind, res.GVR.Version, res.Kind, msg))
	}
	if tm.Kind != "" && tm.Kind != res.Kind {
		return nil, nil, cannot(fmt.Sprintf("kind %q does not match %q", tm.Kind, res.Kind))
	}
	strictErrs, err := kjson.UnmarshalStrict(data, obj)
	if err != nil {
		return nil, nil, cannot(err.Error())
	}
	var msgs []string
	for _, e := range strictErrs {
		msgs = append(msgs, e.Error())
	}
	switch validation {
	case "Ignore":
		msgs = nil
	case "Warn", "":
		// Warnings are returned to the client.
	default: // Strict
		if len(msgs) > 0 {
			return nil, nil, cannot("strict decoding error: " + strings.Join(msgs, ", "))
		}
	}
	return obj, msgs, nil
}

// selectors

func listOptions(r *http.Request) (labels.Selector, fields.Selector, error) {
	q := r.URL.Query()
	ls := labels.Everything()
	if v := q.Get("labelSelector"); v != "" {
		s, err := labels.Parse(v)
		if err != nil {
			return nil, nil, apierrors.NewBadRequest(err.Error())
		}
		ls = s
	}
	fs := fields.Everything()
	if v := q.Get("fieldSelector"); v != "" {
		s, err := fields.ParseSelector(v)
		if err != nil {
			return nil, nil, apierrors.NewBadRequest(err.Error())
		}
		fs = s
	}
	return ls, fs, nil
}

// fieldValue resolves a dotted field path ("spec.nodeName") on an object.
func fieldValue(obj runtime.Object, path string) string {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return ""
	}
	var cur any = u
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	switch v := cur.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func matches(obj runtime.Object, ls labels.Selector, fs fields.Selector) bool {
	m, err := meta.Accessor(obj)
	if err != nil {
		return false
	}
	if !ls.Matches(labels.Set(m.GetLabels())) {
		return false
	}
	if fs.Empty() {
		return true
	}
	for _, req := range fs.Requirements() {
		v := fieldValue(obj, req.Field)
		switch req.Operator {
		case "=", "==":
			if v != req.Value {
				return false
			}
		case "!=":
			if v == req.Value {
				return false
			}
		}
	}
	return true
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request, req *request) {
	ls, fs, err := listOptions(r)
	if err != nil {
		writeStatus(w, err)
		return
	}
	objs, rv := s.Cluster.Store.List(req.res.GVR, req.namespace)
	var items []runtime.Object
	for _, o := range objs {
		if matches(o, ls, fs) {
			items = append(items, o)
		}
	}
	if wantsTable(r) {
		writeJSON(w, http.StatusOK, s.toTable(req.res, items, r, true, strconv.FormatUint(rv, 10)))
		return
	}
	list := map[string]any{
		"kind": req.res.ListKind, "apiVersion": req.res.GVR.GroupVersion().String(),
		"metadata": map[string]any{"resourceVersion": strconv.FormatUint(rv, 10)},
	}
	enc := make([]runtime.Object, 0, len(items))
	for _, o := range items {
		enc = append(enc, encodeObject(req.res, o))
	}
	list["items"] = enc
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) serveDeleteCollection(w http.ResponseWriter, r *http.Request, req *request, dryRun bool) {
	ls, fs, err := listOptions(r)
	if err != nil {
		writeStatus(w, err)
		return
	}
	prop := deletePropagation(r)
	objs, rv := s.Cluster.Store.List(req.res.GVR, req.namespace)
	var deleted []runtime.Object
	for _, o := range objs {
		if !matches(o, ls, fs) {
			continue
		}
		m, _ := meta.Accessor(o)
		if req.res.Kind == "Namespace" {
			continue
		}
		if out, err := s.Cluster.Delete(req.res, m.GetNamespace(), m.GetName(), prop, dryRun); err == nil {
			deleted = append(deleted, encodeObject(req.res, out))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": req.res.ListKind, "apiVersion": req.res.GVR.GroupVersion().String(),
		"metadata": map[string]any{"resourceVersion": strconv.FormatUint(rv, 10)}, "items": deleted,
	})
}

// serveWatch streams watch events as newline-delimited JSON.
func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request, req *request) {
	ls, fs, err := listOptions(r)
	if err != nil {
		writeStatus(w, err)
		return
	}
	q := r.URL.Query()
	rvStr := q.Get("resourceVersion")
	sendInitial := q.Get("sendInitialEvents") == "true" || rvStr == "" || rvStr == "0"
	var fromRV uint64
	if !sendInitial {
		fromRV, err = strconv.ParseUint(rvStr, 10, 64)
		if err != nil {
			writeStatus(w, apierrors.NewBadRequest("invalid resourceVersion"))
			return
		}
	}
	ch, cancel, err := s.Cluster.Store.Watch(req.res.GVR, req.namespace, fromRV, sendInitial)
	if err != nil {
		writeStatus(w, err)
		return
	}
	defer cancel()
	timeout := 30 * time.Minute
	if ts := q.Get("timeoutSeconds"); ts != "" {
		if n, err := strconv.Atoi(ts); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	table := wantsTable(r)
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, private")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	first := true
	send := func(t watch.EventType, obj runtime.Object) bool {
		var out any = encodeObject(req.res, obj)
		if table && t != watch.Bookmark {
			tbl := s.toTable(req.res, []runtime.Object{obj}, r, first, "")
			first = false
			out = tbl
		}
		if err := enc.Encode(map[string]any{"type": t, "object": out}); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	initialPending := sendInitial && q.Get("sendInitialEvents") == "true"
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	idle := time.NewTimer(50 * time.Millisecond)
	defer idle.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			return
		case <-idle.C:
			if initialPending {
				initialPending = false
				bm := req.res.New()
				m, _ := meta.Accessor(bm)
				m.SetResourceVersion(strconv.FormatUint(s.Cluster.Store.ResourceVersion(), 10))
				m.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
				if !send(watch.Bookmark, bm) {
					return
				}
			}
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !matches(e.Object, ls, fs) && e.Type != watch.Deleted {
				continue
			}
			if e.Type == watch.Deleted && !matches(e.Object, ls, fs) {
				continue
			}
			if !send(e.Type, e.Object) {
				return
			}
			if initialPending {
				idle.Reset(50 * time.Millisecond)
			}
		}
	}
}

// servePatch applies JSON, merge, strategic merge or (simplified)
// server-side apply patches.
func (s *Server) servePatch(w http.ResponseWriter, r *http.Request, req *request, dryRun bool) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(w, apierrors.NewRequestEntityTooLargeError(err.Error()))
		return
	}
	pt := types.PatchType(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	validation := r.URL.Query().Get("fieldValidation")
	var warnings []string
	if pt == types.ApplyYAMLPatchType || pt == types.ApplyCBORPatchType {
		if pt == types.ApplyCBORPatchType {
			writeStatus(w, unsupportedMediaType(string(pt)))
			return
		}
		jdata, err := yaml.YAMLToJSON(data)
		if err != nil {
			writeStatus(w, apierrors.NewBadRequest(err.Error()))
			return
		}
		data = jdata
		// Server-side apply (simplified): create if missing, otherwise merge.
		if _, err := s.Cluster.Store.Get(req.res.GVR, req.namespace, req.name); apierrors.IsNotFound(err) && req.subresource == "" {
			obj, warns, err := decodeJSON(validation, req.res, data)
			if err != nil {
				writeStatus(w, err)
				return
			}
			setWarnings(w, warns)
			m, _ := meta.Accessor(obj)
			m.SetName(req.name)
			if req.res.Namespaced {
				m.SetNamespace(req.namespace)
			}
			out, err := s.Cluster.Create(req.res, obj, dryRun)
			if err != nil {
				writeStatus(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, encodeObject(req.res, out))
			return
		}
		pt = types.StrategicMergePatchType
	}
	out, err := s.Cluster.Mutate(req.res, req.namespace, req.name, req.subresource, dryRun, func(cur runtime.Object) (runtime.Object, error) {
		orig, err := json.Marshal(encodeObject(req.res, cur))
		if err != nil {
			return nil, err
		}
		var patched []byte
		switch pt {
		case types.JSONPatchType:
			p, err := jsonpatch.DecodePatch(data)
			if err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
			patched, err = p.Apply(orig)
			if err != nil {
				return nil, apierrors.NewBadRequest("the JSON patch could not be applied: " + err.Error())
			}
		case types.MergePatchType:
			patched, err = jsonpatch.MergePatch(orig, data)
			if err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
		case types.StrategicMergePatchType:
			patched, err = strategicpatch.StrategicMergePatch(orig, data, req.res.New())
			if err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
		default:
			return nil, unsupportedMediaType(string(pt))
		}
		obj, warns, err := decodeJSON(validation, req.res, patched)
		if err != nil {
			return nil, err
		}
		warnings = warns
		m, _ := meta.Accessor(obj)
		// Patches do not carry optimistic concurrency unless explicitly set.
		cm, _ := meta.Accessor(cur)
		if m.GetResourceVersion() == cm.GetResourceVersion() {
			m.SetResourceVersion("")
		}
		return obj, nil
	})
	if err != nil {
		writeStatus(w, err)
		return
	}
	setWarnings(w, warnings)
	writeJSON(w, http.StatusOK, encodeObject(req.res, out))
}

// serveScale implements the scale subresource for deployments and replicasets.
func (s *Server) serveScale(w http.ResponseWriter, r *http.Request, req *request, dryRun bool) {
	toScale := func(obj runtime.Object) *autoscalingv1.Scale {
		sc := &autoscalingv1.Scale{TypeMeta: metav1.TypeMeta{Kind: "Scale", APIVersion: "autoscaling/v1"}}
		m, _ := meta.Accessor(obj)
		sc.ObjectMeta = metav1.ObjectMeta{Name: m.GetName(), Namespace: m.GetNamespace(), UID: m.GetUID(),
			ResourceVersion: m.GetResourceVersion(), CreationTimestamp: m.GetCreationTimestamp()}
		switch o := obj.(type) {
		case *appsv1.Deployment:
			sc.Spec.Replicas = *o.Spec.Replicas
			sc.Status.Replicas = o.Status.Replicas
			sel, _ := metav1.LabelSelectorAsSelector(o.Spec.Selector)
			sc.Status.Selector = sel.String()
		case *appsv1.ReplicaSet:
			sc.Spec.Replicas = *o.Spec.Replicas
			sc.Status.Replicas = o.Status.Replicas
			sel, _ := metav1.LabelSelectorAsSelector(o.Spec.Selector)
			sc.Status.Selector = sel.String()
		}
		return sc
	}
	if r.Method == http.MethodGet {
		obj, err := s.Cluster.Store.Get(req.res.GVR, req.namespace, req.name)
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toScale(obj))
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(w, err)
		return
	}
	pt := types.PatchType(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	out, err := s.Cluster.Mutate(req.res, req.namespace, req.name, "", dryRun, func(cur runtime.Object) (runtime.Object, error) {
		sc := toScale(cur)
		orig, _ := json.Marshal(sc)
		var next []byte
		switch {
		case r.Method == http.MethodPut:
			next = data
		case pt == types.JSONPatchType:
			p, err := jsonpatch.DecodePatch(data)
			if err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
			if next, err = p.Apply(orig); err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
		default:
			if next, err = jsonpatch.MergePatch(orig, data); err != nil {
				return nil, apierrors.NewBadRequest(err.Error())
			}
		}
		var want autoscalingv1.Scale
		if err := json.Unmarshal(next, &want); err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		if want.ResourceVersion != "" && want.ResourceVersion != sc.ResourceVersion {
			return nil, apierrors.NewConflict(req.res.GVR.GroupResource(), req.name, fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
		}
		if want.Spec.Replicas < 0 {
			return nil, apierrors.NewInvalid(schema.GroupKind{Group: "autoscaling", Kind: "Scale"}, req.name, nil)
		}
		switch o := cur.(type) {
		case *appsv1.Deployment:
			o.Spec.Replicas = &want.Spec.Replicas
		case *appsv1.ReplicaSet:
			o.Spec.Replicas = &want.Spec.Replicas
		}
		m, _ := meta.Accessor(cur)
		m.SetResourceVersion("")
		return cur, nil
	})
	if err != nil {
		writeStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toScale(out))
}

func (s *Server) serveLogs(w http.ResponseWriter, r *http.Request, req *request) {
	q := r.URL.Query()
	lines, err := s.Cluster.Logs(req.namespace, req.name, q.Get("container"), q.Get("previous") == "true")
	if err != nil {
		writeStatus(w, err)
		return
	}
	if t := q.Get("tailLines"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n >= 0 && n < len(lines) {
			lines = lines[len(lines)-n:]
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	for _, l := range lines {
		_, _ = io.WriteString(w, l+"\n")
	}
	if q.Get("follow") == "true" {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Simulated processes produce no further output; keep the stream
		// open like a quiet container until the client disconnects.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Minute):
		}
	}
}

var protoSerializer = protobuf.NewSerializer(scheme.Scheme, scheme.Scheme)

// decodeProtobuf decodes Kubernetes protobuf bodies (used by typed clients
// such as `kubectl create deployment`).
func decodeProtobuf(res *registry.Resource, data []byte) (runtime.Object, []string, error) {
	into := res.New()
	obj, gvk, err := protoSerializer.Decode(data, nil, into)
	if err != nil {
		return nil, nil, apierrors.NewBadRequest("invalid protobuf body: " + err.Error())
	}
	if gvk != nil && (gvk.Kind != res.Kind || gvk.Group != res.GVR.Group) {
		return nil, nil, apierrors.NewBadRequest(fmt.Sprintf("%s in version %q cannot be handled as a %s", gvk.Kind, gvk.Version, res.Kind))
	}
	return obj, nil, nil
}
