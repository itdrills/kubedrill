package imagecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRegistry imitates Docker Hub: anonymous bearer tokens, 401 for unknown
// repositories, 404 for unknown tags, 200 + digest for known tags.
func fakeRegistry(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var heads atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if !strings.HasPrefix(r.URL.Query().Get("scope"), "repository:") {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"token":"anon-` + r.URL.Query().Get("scope") + `"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			if r.Method != http.MethodHead {
				http.Error(w, "only HEAD expected", http.StatusMethodNotAllowed)
				return
			}
			heads.Add(1)
			repo, ref, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
			if r.Header.Get("Authorization") != "Bearer anon-repository:"+repo+":pull" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:`+repo+`:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch {
			case repo == "library/nginx" && (ref == "1.27" || ref == "sha256:abc"):
				w.Header().Set("Docker-Content-Digest", "sha256:abc")
				w.WriteHeader(http.StatusOK)
			case repo == "library/nginx":
				w.WriteHeader(http.StatusNotFound)
			case repo == "library/busy":
				w.WriteHeader(http.StatusServiceUnavailable)
			default: // unknown repository: Docker Hub answers 401 even with a token
				w.WriteHeader(http.StatusUnauthorized)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &heads
}

func newChecker(t *testing.T, srv *httptest.Server) *Checker {
	c := New(filepath.Join(t.TempDir(), "cache.json"))
	c.Endpoint = func(string) string { return srv.URL }
	c.Timeout = 2 * time.Second
	return c
}

func TestCheckOutcomes(t *testing.T) {
	srv, _ := fakeRegistry(t)
	c := newChecker(t, srv)
	ctx := context.Background()
	for _, tc := range []struct {
		repo, ref string
		want      Status
	}{
		{"library/nginx", "1.27", Exists},
		{"library/nginx", "sha256:abc", Exists},
		{"library/nginx", "9.99", NotFound},
		{"library/ngnix", "latest", Denied},
		{"library/busy", "1", Unreachable},
	} {
		got := c.Check(ctx, "docker.io", tc.repo, tc.ref)
		if got.Status != tc.want {
			t.Errorf("%s:%s = %s (%s), want %s", tc.repo, tc.ref, got.Status, got.Detail, tc.want)
		}
	}
	if r := c.Check(ctx, "docker.io", "library/nginx", "1.27"); r.Digest != "sha256:abc" {
		t.Errorf("digest = %q", r.Digest)
	}
}

func TestUnreachableRegistry(t *testing.T) {
	srv, _ := fakeRegistry(t)
	c := newChecker(t, srv)
	srv.Close()
	if r := c.Check(context.Background(), "docker.io", "library/nginx", "1.27"); r.Status != Unreachable {
		t.Fatalf("closed server: %s", r.Status)
	}
}

func TestBasicAuthIsDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newChecker(t, srv)
	if r := c.Check(context.Background(), "private.example", "team/app", "1"); r.Status != Denied {
		t.Fatalf("basic auth registry: %s", r.Status)
	}
}

func TestLookupIsAsyncCachedAndPersisted(t *testing.T) {
	srv, heads := fakeRegistry(t)
	c := newChecker(t, srv)
	if r := c.Lookup("docker.io", "library/nginx", "1.27"); r.Status != Pending {
		t.Fatalf("first lookup must be pending, got %s", r.Status)
	}
	var r Result
	for i := 0; i < 200; i++ {
		if r = c.Lookup("docker.io", "library/nginx", "1.27"); r.Status != Pending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Status != Exists {
		t.Fatalf("lookup result %s", r.Status)
	}
	before := heads.Load()
	c.Lookup("docker.io", "library/nginx", "1.27")
	if heads.Load() != before {
		t.Fatalf("cached result must not hit the registry again")
	}
	// A new checker reads the persisted cache: no network needed.
	c2 := New(c.cachePath)
	c2.Endpoint = func(string) string { return "http://127.0.0.1:1" }
	if r := c2.Lookup("docker.io", "library/nginx", "1.27"); r.Status != Exists {
		t.Fatalf("persisted cache not used: %s", r.Status)
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:library/nginx:pull" {
		t.Fatalf("got %s %v", s, p)
	}
}
