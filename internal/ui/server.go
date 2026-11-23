// Package ui serves the local browser UI (`kubedrill ui`): a small web page on
// 127.0.0.1 with a restricted command line, the task of the active challenge,
// hints and a check button. It is a thin client of the admin API, like the CLI.
//
// Security model: the page is reachable only from this machine (loopback
// listener), every request must carry the random session token (cookie set
// from the one-time URL), the Host header must name the loopback address
// (blocks DNS rebinding) and state-changing requests must come from the page
// itself (Origin check, SameSite=Strict cookie, no CORS headers). The command
// line never reaches a shell; see Parse.
package ui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"
)

//go:embed assets
var assets embed.FS

// CookieName holds the session token in the browser. Cookies are not
// separated by port, so the port is part of the name.
func (s *Server) CookieName() string { return "kubedrill_ui_" + strconv.Itoa(s.Port) }

// MaxOutput caps the output of one command shown in the browser.
const MaxOutput = 512 << 10

// RunTimeout bounds one command; the terminal is not interactive.
const RunTimeout = 30 * time.Second

// Backend connects the UI to the simulator.
type Backend interface {
	// State returns the catalog and the active challenge (nil if none) as JSON-able values.
	State() (catalog any, challenge any, err error)
	// Run executes a validated command and returns its combined output and exit code.
	Run(ctx context.Context, cmd *Command) (output string, exitCode int, err error)
}

// Server is the HTTP handler of the browser UI.
type Server struct {
	Token   string
	Port    int
	Backend Backend
}

// RunRequest is the body of POST /api/run.
type RunRequest struct {
	Line string `json:"line"`
}

// RunResponse is the answer of POST /api/run.
type RunResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	Rejected bool   `json:"rejected,omitempty"`
}

// StateResponse is the answer of GET /api/state.
type StateResponse struct {
	Running   bool   `json:"running"`
	Error     string `json:"error,omitempty"`
	Catalog   any    `json:"catalog,omitempty"`
	Challenge any    `json:"challenge,omitempty"`
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(assets, "assets")
	files := http.FileServer(http.FS(static))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/run", s.run)
	mux.Handle("GET /", files)
	return s.guard(mux)
}

func (s *Server) allowedHosts() []string {
	p := strconv.Itoa(s.Port)
	return []string{"127.0.0.1:" + p, "localhost:" + p}
}

// guard enforces host, token and origin checks on every request.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")

		if !contains(s.allowedHosts(), r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// One-time link: ?token=… sets the cookie and redirects to a clean URL,
		// so the token does not stay in the address bar or the history.
		if t := r.URL.Query().Get("token"); t != "" && r.Method == http.MethodGet {
			if !s.validToken(t) {
				http.Error(w, "invalid token – open the link printed by `kubedrill ui`", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: s.CookieName(), Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(s.CookieName())
		if err != nil || !s.validToken(c.Value) {
			http.Error(w, "not authorized – open the link printed by `kubedrill ui`", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin == "" || !contains([]string{"http://" + s.allowedHosts()[0], "http://" + s.allowedHosts()[1]}, origin) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validToken(t string) bool {
	return len(s.Token) >= 32 && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	catalog, challenge, err := s.Backend.State()
	resp := StateResponse{Running: err == nil, Catalog: catalog, Challenge: challenge}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxLineLength+1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, RunResponse{Output: "bad request: " + err.Error(), ExitCode: 2, Rejected: true})
		return
	}
	cmd, err := Parse(req.Line)
	if err != nil {
		writeJSON(w, http.StatusOK, RunResponse{Output: err.Error() + "\n", ExitCode: 2, Rejected: true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), RunTimeout)
	defer cancel()
	out, code, err := s.Backend.Run(ctx, cmd)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			out += "\n(stopped after " + RunTimeout.String() + ")\n"
		} else {
			out += err.Error() + "\n"
		}
		if code == 0 {
			code = 1
		}
	}
	if len(out) > MaxOutput {
		out = out[:MaxOutput] + "\n… (output truncated)\n"
	}
	writeJSON(w, http.StatusOK, RunResponse{Output: out, ExitCode: code})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Listen opens the loopback listener (port 0 = any free port).
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
