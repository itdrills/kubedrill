// Package registry describes the subset of Kubernetes resources the simulator
// serves. It is the single source of truth for discovery, REST mapping and
// object instantiation.
package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// Resource describes one served resource type.
type Resource struct {
	GVR          schema.GroupVersionResource
	Kind         string
	ListKind     string
	Singular     string
	Namespaced   bool
	ShortNames   []string
	Categories   []string
	Subresources []string
	// ReadOnly resources are maintained by the simulation (e.g. endpoints)
	// but users may still read them.
	New     func() runtime.Object
	NewList func() runtime.Object
}

// GVK returns the group/version/kind of the resource.
func (r *Resource) GVK() schema.GroupVersionKind {
	return r.GVR.GroupVersion().WithKind(r.Kind)
}

var standardVerbs = []string{"create", "delete", "deletecollection", "get", "list", "patch", "update", "watch"}

// Verbs returns the verbs advertised in discovery.
func (r *Resource) Verbs() []string { return standardVerbs }

var all = []*Resource{
	{GVR: corev1.SchemeGroupVersion.WithResource("namespaces"), Kind: "Namespace", ListKind: "NamespaceList", Singular: "namespace", ShortNames: []string{"ns"}, Subresources: []string{"status"},
		New: func() runtime.Object { return &corev1.Namespace{} }, NewList: func() runtime.Object { return &corev1.NamespaceList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("nodes"), Kind: "Node", ListKind: "NodeList", Singular: "node", ShortNames: []string{"no"}, Subresources: []string{"status"},
		New: func() runtime.Object { return &corev1.Node{} }, NewList: func() runtime.Object { return &corev1.NodeList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("pods"), Kind: "Pod", ListKind: "PodList", Singular: "pod", Namespaced: true, ShortNames: []string{"po"}, Categories: []string{"all"}, Subresources: []string{"status", "log", "exec", "portforward"},
		New: func() runtime.Object { return &corev1.Pod{} }, NewList: func() runtime.Object { return &corev1.PodList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("services"), Kind: "Service", ListKind: "ServiceList", Singular: "service", Namespaced: true, ShortNames: []string{"svc"}, Categories: []string{"all"}, Subresources: []string{"status"},
		New: func() runtime.Object { return &corev1.Service{} }, NewList: func() runtime.Object { return &corev1.ServiceList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("endpoints"), Kind: "Endpoints", ListKind: "EndpointsList", Singular: "endpoints", Namespaced: true, ShortNames: []string{"ep"},
		New: func() runtime.Object { return &corev1.Endpoints{} }, NewList: func() runtime.Object { return &corev1.EndpointsList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("configmaps"), Kind: "ConfigMap", ListKind: "ConfigMapList", Singular: "configmap", Namespaced: true, ShortNames: []string{"cm"},
		New: func() runtime.Object { return &corev1.ConfigMap{} }, NewList: func() runtime.Object { return &corev1.ConfigMapList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("secrets"), Kind: "Secret", ListKind: "SecretList", Singular: "secret", Namespaced: true,
		New: func() runtime.Object { return &corev1.Secret{} }, NewList: func() runtime.Object { return &corev1.SecretList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("serviceaccounts"), Kind: "ServiceAccount", ListKind: "ServiceAccountList", Singular: "serviceaccount", Namespaced: true, ShortNames: []string{"sa"},
		New: func() runtime.Object { return &corev1.ServiceAccount{} }, NewList: func() runtime.Object { return &corev1.ServiceAccountList{} }},
	{GVR: corev1.SchemeGroupVersion.WithResource("events"), Kind: "Event", ListKind: "EventList", Singular: "event", Namespaced: true, ShortNames: []string{"ev"},
		New: func() runtime.Object { return &corev1.Event{} }, NewList: func() runtime.Object { return &corev1.EventList{} }},
	{GVR: appsv1.SchemeGroupVersion.WithResource("deployments"), Kind: "Deployment", ListKind: "DeploymentList", Singular: "deployment", Namespaced: true, ShortNames: []string{"deploy"}, Categories: []string{"all"}, Subresources: []string{"status", "scale"},
		New: func() runtime.Object { return &appsv1.Deployment{} }, NewList: func() runtime.Object { return &appsv1.DeploymentList{} }},
	{GVR: appsv1.SchemeGroupVersion.WithResource("replicasets"), Kind: "ReplicaSet", ListKind: "ReplicaSetList", Singular: "replicaset", Namespaced: true, ShortNames: []string{"rs"}, Categories: []string{"all"}, Subresources: []string{"status", "scale"},
		New: func() runtime.Object { return &appsv1.ReplicaSet{} }, NewList: func() runtime.Object { return &appsv1.ReplicaSetList{} }},
	{GVR: discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), Kind: "EndpointSlice", ListKind: "EndpointSliceList", Singular: "endpointslice", Namespaced: true,
		New: func() runtime.Object { return &discoveryv1.EndpointSlice{} }, NewList: func() runtime.Object { return &discoveryv1.EndpointSliceList{} }},
	{GVR: networkingv1.SchemeGroupVersion.WithResource("ingresses"), Kind: "Ingress", ListKind: "IngressList", Singular: "ingress", Namespaced: true, ShortNames: []string{"ing"}, Subresources: []string{"status"},
		New: func() runtime.Object { return &networkingv1.Ingress{} }, NewList: func() runtime.Object { return &networkingv1.IngressList{} }},
	{GVR: networkingv1.SchemeGroupVersion.WithResource("ingressclasses"), Kind: "IngressClass", ListKind: "IngressClassList", Singular: "ingressclass",
		New: func() runtime.Object { return &networkingv1.IngressClass{} }, NewList: func() runtime.Object { return &networkingv1.IngressClassList{} }},
}

var (
	byGVR  = map[schema.GroupVersionResource]*Resource{}
	byKind = map[schema.GroupKind]*Resource{}
)

func init() {
	for _, r := range all {
		byGVR[r.GVR] = r
		byKind[r.GVK().GroupKind()] = r
	}
}

// All returns every served resource.
func All() []*Resource { return all }

// Lookup finds a resource by group/version/resource.
func Lookup(gvr schema.GroupVersionResource) (*Resource, bool) {
	r, ok := byGVR[gvr]
	return r, ok
}

// LookupKind finds a resource by group and kind.
func LookupKind(gk schema.GroupKind) (*Resource, bool) {
	r, ok := byKind[gk]
	return r, ok
}

// GroupVersions returns all served group versions, sorted, core first.
func GroupVersions() []schema.GroupVersion {
	seen := map[schema.GroupVersion]bool{}
	var out []schema.GroupVersion
	for _, r := range all {
		gv := r.GVR.GroupVersion()
		if !seen[gv] {
			seen[gv] = true
			out = append(out, gv)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group == "" {
			return true
		}
		if out[j].Group == "" {
			return false
		}
		return out[i].Group < out[j].Group
	})
	return out
}

// ForGroupVersion returns resources of one group version.
func ForGroupVersion(gv schema.GroupVersion) []*Resource {
	var out []*Resource
	for _, r := range all {
		if r.GVR.GroupVersion() == gv {
			out = append(out, r)
		}
	}
	return out
}

// Decode turns a single JSON or YAML manifest into a typed object of a
// served resource. Unknown fields are reported as errors (strict decoding).
func Decode(data []byte) (*Resource, runtime.Object, error) {
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var tm metav1.TypeMeta
	if err := json.Unmarshal(j, &tm); err != nil {
		return nil, nil, fmt.Errorf("invalid manifest: %w", err)
	}
	if tm.APIVersion == "" || tm.Kind == "" {
		return nil, nil, fmt.Errorf("manifest needs apiVersion and kind")
	}
	gv, err := schema.ParseGroupVersion(tm.APIVersion)
	if err != nil {
		return nil, nil, err
	}
	r, ok := LookupKind(schema.GroupKind{Group: gv.Group, Kind: tm.Kind})
	if !ok || r.GVR.Version != gv.Version {
		return nil, nil, fmt.Errorf("%s %s is not supported by the simulator", tm.APIVersion, tm.Kind)
	}
	obj := r.New()
	strictErrs, err := kjson.UnmarshalStrict(j, obj)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", tm.Kind, err)
	}
	if len(strictErrs) > 0 {
		var msgs []string
		for _, e := range strictErrs {
			msgs = append(msgs, e.Error())
		}
		return nil, nil, fmt.Errorf("%s: strict decoding error: %s", tm.Kind, strings.Join(msgs, ", "))
	}
	return r, obj, nil
}

// SplitYAML splits a multi-document YAML stream, skipping empty documents.
func SplitYAML(data []byte) [][]byte {
	var out [][]byte
	for _, doc := range strings.Split("\n"+string(data), "\n---") {
		doc = strings.TrimSpace(doc)
		lines := strings.Split(doc, "\n")
		empty := true
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if t != "" && !strings.HasPrefix(t, "#") {
				empty = false
				break
			}
		}
		if !empty {
			out = append(out, []byte(doc))
		}
	}
	return out
}
