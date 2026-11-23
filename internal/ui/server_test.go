package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeBackend struct {
	ran      []*Command
	stateErr error
}

func (f *fakeBackend) State() (any, any, error) {
	if f.stateErr != nil {
		return nil, nil, f.stateErr
	}
	return map[string]any{"scenarios": []any{}}, nil, nil
}

func (f *fakeBackend) Run(_ context.Context, cmd *Command) (string, int, error) {
	f.ran = append(f.ran, cmd)
	return "ok\n", 0, nil
}

func newTestServer() (*Server, *fakeBackend) {
	b := &fakeBackend{}
	return &Server{Token: testToken, Port: 4711, Backend: b}, b
}

func do(t *testing.T, s *Server, method, target, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "127.0.0.1:4711"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func withCookie(s *Server) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: s.CookieName(), Value: testToken}) }
}

func TestTokenLinkSetsCookieAndRedirects(t *testing.T) {
	s, _ := newTestServer()
	rec := do(t, s, "GET", "/?token="+testToken, "", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("code %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Value != testToken || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", c)
	}
	if rec := do(t, s, "GET", "/?token=wrong", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", rec.Code)
	}
}

func TestRequiresCookie(t *testing.T) {
	s, _ := newTestServer()
	for _, p := range []string{"/", "/app.js", "/api/state"} {
		if rec := do(t, s, "GET", p, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without cookie: %d", p, rec.Code)
		}
	}
	bad := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: s.CookieName(), Value: "guess"}) }
	if rec := do(t, s, "GET", "/api/state", "", bad); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie: %d", rec.Code)
	}
	rec := do(t, s, "GET", "/", "", withCookie(s))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "KubeDrill") {
		t.Fatalf("index: %d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP %q", csp)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS header must not be set")
	}
}

func TestRejectsForeignHost(t *testing.T) {
	s, _ := newTestServer()
	// DNS rebinding: evil.example resolves to 127.0.0.1, the browser sends its name.
	for _, host := range []string{"evil.example:4711", "127.0.0.1:9999", "0.0.0.0:4711", ""} {
		rec := do(t, s, "GET", "/api/state", "", func(r *http.Request) {
			withCookie(s)(r)
			r.Host = host
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("host %q: %d", host, rec.Code)
		}
	}
	if rec := do(t, s, "GET", "/api/state", "", func(r *http.Request) {
		withCookie(s)(r)
		r.Host = "localhost:4711"
	}); rec.Code != http.StatusOK {
		t.Errorf("localhost: %d", rec.Code)
	}
}

func TestRunChecksOrigin(t *testing.T) {
	s, b := newTestServer()
	body := `{"line":"kubectl get pods"}`
	for _, origin := range []string{"", "https://evil.example", "http://127.0.0.1:9999", "null"} {
		rec := do(t, s, "POST", "/api/run", body, func(r *http.Request) {
			withCookie(s)(r)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("origin %q: %d", origin, rec.Code)
		}
	}
	if len(b.ran) != 0 {
		t.Fatalf("commands ran: %v", b.ran)
	}
	rec := do(t, s, "POST", "/api/run", body, func(r *http.Request) {
		withCookie(s)(r)
		r.Header.Set("Origin", "http://127.0.0.1:4711")
	})
	var resp RunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Output != "ok\n" || len(b.ran) != 1 {
		t.Fatalf("run: %d %s %v", rec.Code, rec.Body, err)
	}
}

func TestRunRejectsInvalidCommandWithoutRunning(t *testing.T) {
	s, b := newTestServer()
	rec := do(t, s, "POST", "/api/run", `{"line":"rm -rf /"}`, func(r *http.Request) {
		withCookie(s)(r)
		r.Header.Set("Origin", "http://localhost:4711")
	})
	var resp RunResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Rejected || resp.ExitCode == 0 || len(b.ran) != 0 {
		t.Fatalf("resp %+v ran %v", resp, b.ran)
	}
	rec = do(t, s, "POST", "/api/run", `{"line":"kubectl get pods","extra":1}`, func(r *http.Request) {
		withCookie(s)(r)
		r.Header.Set("Origin", "http://localhost:4711")
	})
	if rec.Code != http.StatusBadRequest || len(b.ran) != 0 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
}

func TestStateWhenSimulatorStopped(t *testing.T) {
	s, b := newTestServer()
	b.stateErr = errors.New("simulator is not running")
	rec := do(t, s, "GET", "/api/state", "", withCookie(s))
	var resp StateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Running || resp.Error == "" {
		t.Fatalf("state %+v %v", resp, err)
	}
}

func TestShortTokenNeverValid(t *testing.T) {
	s := &Server{Token: "", Port: 4711, Backend: &fakeBackend{}}
	if s.validToken("") {
		t.Fatal("empty token accepted")
	}
}
