//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestBrowserUI drives `kubedrill ui` over HTTP like the page does.
func TestBrowserUI(t *testing.T) {
	e := setup(t)
	e.sim("start", "--port", fmt.Sprint(e.port), "--time-scale", "2")

	uiPort := freePort(t)
	cmd := exec.Command(e.bin, "ui", "--no-open", "--port", fmt.Sprint(uiPort))
	cmd.Env = e.environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	link := ""
	sc := bufio.NewScanner(stdout)
	re := regexp.MustCompile(`http://127\.0\.0\.1:\d+/\?token=[0-9a-f]{64}`)
	for sc.Scan() {
		if m := re.FindString(sc.Text()); m != "" {
			link = m
			break
		}
	}
	if link == "" {
		t.Fatal("no UI link printed")
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", uiPort)

	// Without the token nothing is served.
	if res, err := http.Get(base + "/api/state"); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("state without token: %v %v", res, err)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 40 * time.Second}
	res, err := client.Get(link)
	if err != nil || res.StatusCode != http.StatusOK || res.Request.URL.RawQuery != "" {
		t.Fatalf("token link: %v %v", res, err)
	}
	_ = res.Body.Close()

	run := func(line string) (string, int, bool) {
		t.Helper()
		req, _ := http.NewRequest("POST", base+"/api/run", strings.NewReader(fmt.Sprintf(`{"line":%q}`, line)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var r struct {
			Output   string `json:"output"`
			ExitCode int    `json:"exitCode"`
			Rejected bool   `json:"rejected"`
		}
		if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		return r.Output, r.ExitCode, r.Rejected
	}

	out, code, _ := run("kubectl get nodes")
	if code != 0 {
		t.Fatalf("kubectl get nodes: %d %s", code, out)
	}
	mustContain(t, out, "kubedrill-control-plane", "Ready")
	out, _, _ = run("kubedrill challenge start image-pull-backoff")
	mustContain(t, out, "Challenge started")

	res, err = client.Get(base + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	mustContain(t, string(body), `"running":true`, `"id":"image-pull-backoff"`, `"criteria"`)

	out, code, _ = run("kubedrill challenge check")
	if code == 0 {
		t.Fatalf("check must fail before the fix:\n%s", out)
	}
	mustContain(t, out, "Not solved yet")
	mustContain(t, func() string { o, _, _ := run("kubedrill hint"); return o }(), "Hint 1/4")

	for _, line := range []string{"id", "kubectl get pods | sh", "kubectl --context prod get pods", "kubedrill stop", "kubectl myplugin"} {
		if _, _, rejected := run(line); !rejected {
			t.Errorf("%q was not rejected", line)
		}
	}
	// A cross-site request is refused even with the cookie.
	req, _ := http.NewRequest("POST", base+"/api/run", strings.NewReader(`{"line":"kubectl get pods"}`))
	req.Header.Set("Origin", "https://evil.example")
	if res, err := client.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin run: %v %v", res, err)
	}

	mustContain(t, func() string { o, _, _ := run("kubectl set image deployment/web web=nginx:1.27"); return o }(), "image updated")
	mustContain(t, func() string { o, _, _ := run("kubectl rollout status deployment/web --timeout=25s"); return o }(), "successfully rolled out")
	out, code, _ = run("kubedrill challenge check")
	if code != 0 {
		t.Fatalf("check after fix: %d\n%s", code, out)
	}
	mustContain(t, out, "Solved!")
	mustContain(t, func() string { o, _, _ := run("kubedrill challenge stop"); return o }(), "restored")
	if n := e.prodHits.Load(); n != 0 {
		t.Fatalf("the fake production cluster received %d request(s)", n)
	}
}
