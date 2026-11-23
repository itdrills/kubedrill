package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/itskills/kubedrill/internal/sim"
)

const token = "test-token"

func newServer(t *testing.T) (*httptest.Server, *sim.FakeClock, *sim.Cluster) {
	t.Helper()
	clk := sim.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := sim.New(sim.Options{Clock: clk})
	ts := httptest.NewServer(&Server{Cluster: c, Token: token})
	t.Cleanup(ts.Close)
	return ts, clk, c
}

func do(t *testing.T, ts *httptest.Server, method, path, contentType, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func statusReason(t *testing.T, body string) metav1.StatusReason {
	var st metav1.Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("not a Status: %s", body)
	}
	return st.Reason
}

const deployJSON = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"nginx:1.27"}]}}}}`

func TestUnauthorized(t *testing.T) {
	ts, _, _ := newServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/pods")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if r, _ := http.Get(ts.URL + "/healthz"); r.StatusCode != 200 {
		t.Fatalf("healthz must not require auth")
	}
}

func TestDiscovery(t *testing.T) {
	ts, _, _ := newServer(t)
	code, body := do(t, ts, "GET", "/api", "", "")
	if code != 200 || !strings.Contains(body, `"v1"`) {
		t.Fatalf("/api: %d %s", code, body)
	}
	code, body = do(t, ts, "GET", "/apis", "", "")
	if code != 200 || !strings.Contains(body, `"apps"`) || !strings.Contains(body, "discovery.k8s.io") {
		t.Fatalf("/apis: %d %s", code, body)
	}
	_, body = do(t, ts, "GET", "/api/v1", "", "")
	var rl metav1.APIResourceList
	_ = json.Unmarshal([]byte(body), &rl)
	found := map[string]bool{}
	for _, r := range rl.APIResources {
		found[r.Name] = true
	}
	for _, want := range []string{"pods", "pods/log", "nodes", "services", "events", "configmaps", "secrets", "namespaces"} {
		if !found[want] {
			t.Errorf("/api/v1 misses %s", want)
		}
	}
	for _, gv := range []string{"/apis/apps/v1", "/apis/networking.k8s.io/v1"} {
		if code, _ = do(t, ts, "GET", gv, "", ""); code != 200 {
			t.Fatalf("%s: %d", gv, code)
		}
	}
	if code, _ = do(t, ts, "GET", "/apis/batch/v1", "", ""); code != 404 {
		t.Fatalf("unsupported group should be 404, got %d", code)
	}
	code, body = do(t, ts, "GET", "/openapi/v3", "", "")
	if code != 200 || !strings.Contains(body, "apis/apps/v1") {
		t.Fatalf("/openapi/v3: %d %s", code, body)
	}
	code, body = do(t, ts, "GET", "/version", "", "")
	if code != 200 || !strings.Contains(body, sim.KubernetesVersion) {
		t.Fatalf("/version: %d %s", code, body)
	}
}

func TestCRUDStatusCodes(t *testing.T) {
	ts, _, _ := newServer(t)
	base := "/apis/apps/v1/namespaces/default/deployments"
	if code, body := do(t, ts, "POST", base, "application/json", deployJSON); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	code, body := do(t, ts, "POST", base, "application/json", deployJSON)
	if code != 409 || statusReason(t, body) != metav1.StatusReasonAlreadyExists {
		t.Fatalf("duplicate create: %d %s", code, body)
	}
	if code, _ := do(t, ts, "GET", base+"/web", "", ""); code != 200 {
		t.Fatalf("get: %d", code)
	}
	code, body = do(t, ts, "GET", base+"/nope", "", "")
	if code != 404 || statusReason(t, body) != metav1.StatusReasonNotFound {
		t.Fatalf("get missing: %d %s", code, body)
	}
	// Strict field validation (kubectl default).
	bad := strings.Replace(deployJSON, `"replicas":2`, `"replicaz":2`, 1)
	bad = strings.Replace(bad, `"name":"web"}`, `"name":"web2"}`, 1)
	code, body = do(t, ts, "POST", base+"?fieldValidation=Strict", "application/json", bad)
	if code != 400 || !strings.Contains(body, `unknown field \"spec.replicaz\"`) {
		t.Fatalf("strict validation: %d %s", code, body)
	}
	// Semantic validation: selector must match template labels.
	inv := strings.Replace(deployJSON, `"matchLabels":{"app":"web"}`, `"matchLabels":{"app":"other"}`, 1)
	inv = strings.Replace(inv, `"name":"web"}`, `"name":"web3"}`, 1)
	code, body = do(t, ts, "POST", base, "application/json", inv)
	if code != 422 || statusReason(t, body) != metav1.StatusReasonInvalid {
		t.Fatalf("invalid: %d %s", code, body)
	}
	// Namespace must exist.
	code, _ = do(t, ts, "POST", "/apis/apps/v1/namespaces/missing/deployments", "application/json", deployJSON)
	if code != 404 {
		t.Fatalf("missing namespace: %d", code)
	}
	// Strategic merge patch (kubectl set image).
	code, body = do(t, ts, "PATCH", base+"/web", "application/strategic-merge-patch+json",
		`{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"nginx:1.28"}]}}}}`)
	if code != 200 || !strings.Contains(body, "nginx:1.28") {
		t.Fatalf("smp: %d %s", code, body)
	}
	// JSON patch and merge patch.
	if code, body = do(t, ts, "PATCH", base+"/web", "application/json-patch+json", `[{"op":"replace","path":"/spec/replicas","value":3}]`); code != 200 {
		t.Fatalf("json patch: %d %s", code, body)
	}
	if code, body = do(t, ts, "PATCH", base+"/web", "application/merge-patch+json", `{"metadata":{"labels":{"tier":"web"}}}`); code != 200 {
		t.Fatalf("merge patch: %d %s", code, body)
	}
	// Scale subresource.
	code, body = do(t, ts, "GET", base+"/web/scale", "", "")
	if code != 200 || !strings.Contains(body, `"replicas":3`) {
		t.Fatalf("scale get: %d %s", code, body)
	}
	if code, body = do(t, ts, "PATCH", base+"/web/scale", "application/merge-patch+json", `{"spec":{"replicas":1}}`); code != 200 {
		t.Fatalf("scale patch: %d %s", code, body)
	}
	// Optimistic concurrency.
	code, body = do(t, ts, "PUT", base+"/web", "application/json", strings.Replace(deployJSON, `"name":"web"}`, `"name":"web","resourceVersion":"1"}`, 1))
	if code != 409 || statusReason(t, body) != metav1.StatusReasonConflict {
		t.Fatalf("conflict: %d %s", code, body)
	}
	// Delete cascades to replicasets and pods.
	if code, _ := do(t, ts, "DELETE", base+"/web", "", ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	_, body = do(t, ts, "GET", "/api/v1/namespaces/default/pods", "", "")
	if strings.Contains(body, `"web-`) {
		t.Fatalf("pods not garbage collected: %s", body)
	}
}

func TestTableAndSelectors(t *testing.T) {
	ts, _, _ := newServer(t)
	do(t, ts, "POST", "/apis/apps/v1/namespaces/default/deployments", "application/json", deployJSON)
	code, body := do(t, ts, "GET", "/api/v1/namespaces/default/pods?labelSelector=app%3Dweb", "", "",
		"Accept", "application/json;as=Table;v=v1;g=meta.k8s.io,application/json")
	if code != 200 || !strings.Contains(body, `"kind":"Table"`) || !strings.Contains(body, `"Restarts"`) {
		t.Fatalf("table: %d %s", code, body)
	}
	var tbl metav1.Table
	_ = json.Unmarshal([]byte(body), &tbl)
	if len(tbl.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(tbl.Rows))
	}
	_, body = do(t, ts, "GET", "/api/v1/pods?fieldSelector=metadata.namespace%3Dkube-system", "", "")
	if strings.Contains(body, `"namespace":"default"`) {
		t.Fatalf("field selector not applied")
	}
}

func TestLogsAndExec(t *testing.T) {
	ts, clk, c := newServer(t)
	do(t, ts, "POST", "/apis/apps/v1/namespaces/default/deployments", "application/json", deployJSON)
	for i := 0; i < 10; i++ {
		clk.Advance(time.Second)
		c.Tick()
	}
	objs, _ := c.Store.List(podsGVR(), "default")
	name := objs[0].(interface{ GetName() string }).GetName()
	code, body := do(t, ts, "GET", "/api/v1/namespaces/default/pods/"+name+"/log", "", "")
	if code != 200 || !strings.Contains(body, "nginx") {
		t.Fatalf("logs: %d %s", code, body)
	}
	code, body = do(t, ts, "POST", "/api/v1/namespaces/default/pods/"+name+"/exec", "", "")
	if code != 400 || !strings.Contains(body, "simulated") {
		t.Fatalf("exec: %d %s", code, body)
	}
}

func TestWatch(t *testing.T) {
	ts, _, c := newServer(t)
	rv := c.Store.ResourceVersion()
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/namespaces/default/configmaps?watch=true&resourceVersion="+itoa(rv)+"&timeoutSeconds=5", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/namespaces/default/configmaps",
			strings.NewReader(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"w"},"data":{"a":"b"}}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() {
		t.Fatal("no watch event")
	}
	var ev struct {
		Type   string          `json:"type"`
		Object json.RawMessage `json:"object"`
	}
	_ = json.Unmarshal(sc.Bytes(), &ev)
	if ev.Type != "ADDED" || !strings.Contains(string(ev.Object), `"name":"w"`) {
		t.Fatalf("unexpected event: %s", sc.Text())
	}
}
