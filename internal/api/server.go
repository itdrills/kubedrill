// Package api exposes the simulated cluster through a Kubernetes-compatible
// HTTP API: legacy discovery, OpenAPI v3 (for kubectl explain/validation),
// CRUD, patch (JSON, merge, strategic, simplified server-side apply), watch,
// Table output for kubectl get, logs and the scale subresource.
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/itskills/kubedrill/internal/brand"
	"github.com/itskills/kubedrill/internal/registry"
	"github.com/itskills/kubedrill/internal/sim"
)

// maxBody limits request bodies (manifests) to 4 MiB.
const maxBody = 4 << 20

// Server is the HTTP front-end of a simulated cluster.
type Server struct {
	Cluster *sim.Cluster
	// Token is required as bearer token on every API request (except health).
	Token string
	// Admin handles the simulator's own API below brand.AdminPathPrefix.
	Admin http.Handler
	// Logf receives request logs; nil disables logging.
	Logf func(format string, args ...any)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz", "/livez", "/readyz":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
		return
	}
	if !s.authorized(r) {
		writeStatus(w, &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusUnauthorized,
			Reason: metav1.StatusReasonUnauthorized, Message: "Unauthorized"}})
		return
	}
	if s.Logf != nil {
		s.Logf("%s %s", r.Method, r.URL.RequestURI())
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, brand.AdminPathPrefix+"/") || p == brand.AdminPathPrefix:
		if s.Admin == nil {
			http.NotFound(w, r)
			return
		}
		s.Admin.ServeHTTP(w, r)
	case p == "/version" || p == "/version/":
		writeJSON(w, http.StatusOK, versionInfo())
	case p == "/api" || p == "/api/":
		s.serveAPIVersions(w, r)
	case p == "/apis" || p == "/apis/":
		writeJSON(w, http.StatusOK, apiGroupList())
	case strings.HasPrefix(p, "/openapi/v3"):
		serveOpenAPIV3(w, r)
	case strings.HasPrefix(p, "/api/"):
		s.serveResource(w, r, schema.GroupVersion{Version: "v1"}, strings.TrimPrefix(p, "/api/v1"), strings.HasPrefix(p, "/api/v1"))
	case strings.HasPrefix(p, "/apis/"):
		parts := strings.SplitN(strings.TrimPrefix(p, "/apis/"), "/", 3)
		if len(parts) < 2 {
			if len(parts) == 1 && parts[0] != "" {
				s.serveGroup(w, parts[0])
				return
			}
			writeStatus(w, apierrors.NewNotFound(schema.GroupResource{}, ""))
			return
		}
		rest := ""
		if len(parts) == 3 {
			rest = "/" + parts[2]
		}
		s.serveResource(w, r, schema.GroupVersion{Group: parts[0], Version: parts[1]}, rest, true)
	default:
		writeStatus(w, notFoundPath())
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(s.Token)) == 1
}

func notFoundPath() error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusNotFound,
		Reason: metav1.StatusReasonNotFound, Message: "the server could not find the requested resource",
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Type: metav1.CauseTypeUnexpectedServerResponse, Message: "404 page not found"}}}}}
}

// writeJSON writes v as JSON.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, private")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// writeStatus converts an error into a metav1.Status response.
func writeStatus(w http.ResponseWriter, err error) {
	var st metav1.Status
	if se, ok := err.(apierrors.APIStatus); ok {
		st = se.Status()
	} else {
		st = apierrors.NewInternalError(err).Status()
	}
	st.Kind = "Status"
	st.APIVersion = "v1"
	if st.Status == "" {
		st.Status = metav1.StatusFailure
	}
	code := int(st.Code)
	if code == 0 {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, st)
}

// encodeObject returns a JSON-ready copy of obj with apiVersion/kind set.
func encodeObject(res *registry.Resource, obj runtime.Object) runtime.Object {
	o := obj.DeepCopyObject()
	o.GetObjectKind().SetGroupVersionKind(res.GVK())
	return o
}

func versionInfo() map[string]string {
	return map[string]string{
		"major": "1", "minor": strings.Split(strings.TrimPrefix(sim.KubernetesVersion, "v"), ".")[1],
		"gitVersion":   sim.KubernetesVersion + "+" + brand.Binary,
		"gitCommit":    "0000000000000000000000000000000000000000",
		"gitTreeState": "clean", "buildDate": "2026-01-01T00:00:00Z",
		"goVersion": "go1-simulated", "compiler": "gc", "platform": "linux/arm64",
		"emulationMajor": "1", "emulationMinor": strings.Split(strings.TrimPrefix(sim.KubernetesVersion, "v"), ".")[1],
	}
}

func (s *Server) serveAPIVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": "APIVersions", "versions": []string{"v1"},
		"serverAddressByClientCIDRs": []map[string]string{{"clientCIDR": "0.0.0.0/0", "serverAddress": r.Host}},
	})
}

func apiGroup(group string) *metav1.APIGroup {
	for _, gv := range registry.GroupVersions() {
		if gv.Group == group && group != "" {
			v := metav1.GroupVersionForDiscovery{GroupVersion: gv.String(), Version: gv.Version}
			return &metav1.APIGroup{Name: group, Versions: []metav1.GroupVersionForDiscovery{v}, PreferredVersion: v}
		}
	}
	return nil
}

func apiGroupList() *metav1.APIGroupList {
	l := &metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}}
	for _, gv := range registry.GroupVersions() {
		if gv.Group == "" {
			continue
		}
		l.Groups = append(l.Groups, *apiGroup(gv.Group))
	}
	return l
}

func (s *Server) serveGroup(w http.ResponseWriter, group string) {
	g := apiGroup(group)
	if g == nil {
		writeStatus(w, notFoundPath())
		return
	}
	g.TypeMeta = metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"}
	writeJSON(w, http.StatusOK, g)
}

func resourceList(gv schema.GroupVersion) *metav1.APIResourceList {
	l := &metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: gv.String()}
	for _, r := range registry.ForGroupVersion(gv) {
		l.APIResources = append(l.APIResources, metav1.APIResource{
			Name: r.GVR.Resource, SingularName: r.Singular, Namespaced: r.Namespaced, Kind: r.Kind,
			Verbs: r.Verbs(), ShortNames: r.ShortNames, Categories: r.Categories,
			StorageVersionHash: storageHash(r.GVK().String()),
		})
		for _, sub := range r.Subresources {
			ar := metav1.APIResource{Name: r.GVR.Resource + "/" + sub, Namespaced: r.Namespaced, Kind: r.Kind, Verbs: []string{"get", "patch", "update"}}
			switch sub {
			case "log":
				ar.Verbs = []string{"get"}
				ar.Kind = "Pod"
			case "exec", "portforward":
				ar.Kind = "PodExecOptions"
				if sub == "portforward" {
					ar.Kind = "PodPortForwardOptions"
				}
				ar.Verbs = []string{"create", "get"}
			case "scale":
				ar.Group, ar.Version, ar.Kind = "autoscaling", "v1", "Scale"
			}
			l.APIResources = append(l.APIResources, ar)
		}
	}
	return l
}

func unsupportedMediaType(ct string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusUnsupportedMediaType,
		Reason: metav1.StatusReasonUnsupportedMediaType, Message: fmt.Sprintf("the body of the request was in an unknown format - accepted media types include: application/json-patch+json, application/merge-patch+json, application/apply-patch+yaml, application/strategic-merge-patch+json (got %q)", ct)}}
}

func storageHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:8])
}
