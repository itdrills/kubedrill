package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/itdrills/kubedrill/internal/brand"
	"github.com/itdrills/kubedrill/internal/scenario"
	"github.com/itdrills/kubedrill/internal/sim"
)

// Admin wire types shared with the CLI client.
type (
	// Info identifies a running simulator.
	Info struct {
		Product           string    `json:"product"`
		InstanceID        string    `json:"instanceID"`
		Version           string    `json:"version"`
		KubernetesVersion string    `json:"kubernetesVersion"`
		StartedAt         time.Time `json:"startedAt"`
		// ImageCheck is "online" (registry lookups) or "off".
		ImageCheck string `json:"imageCheck,omitempty"`
	}
	// ScenarioSummary is a catalog entry.
	ScenarioSummary struct {
		ID               string   `json:"id"`
		Title            string   `json:"title"`
		Difficulty       string   `json:"difficulty"`
		EstimatedMinutes int      `json:"estimatedMinutes"`
		Tags             []string `json:"tags"`
		Summary          string   `json:"summary"`
		Source           string   `json:"source"`
		Solved           bool     `json:"solved"`
	}
	// CatalogResponse lists scenarios.
	CatalogResponse struct {
		Scenarios []ScenarioSummary `json:"scenarios"`
		Warnings  []string          `json:"warnings,omitempty"`
		Active    string            `json:"active,omitempty"`
	}
	// ChallengeResponse describes the active challenge.
	ChallengeResponse struct {
		ID               string                `json:"id"`
		Title            string                `json:"title"`
		Difficulty       string                `json:"difficulty"`
		Description      string                `json:"description"`
		Target           scenario.TargetInfo   `json:"target"`
		StartedAt        time.Time             `json:"startedAt"`
		HintsRevealed    int                   `json:"hintsRevealed"`
		HintsTotal       int                   `json:"hintsTotal"`
		SolutionRevealed bool                  `json:"solutionRevealed"`
		Solved           bool                  `json:"solved"`
		SelfSolved       bool                  `json:"selfSolved"`
		SolvedAt         *time.Time            `json:"solvedAt,omitempty"`
		Checks           int                   `json:"checks"`
		Resets           int                   `json:"resets"`
		Check            *scenario.CheckResult `json:"check,omitempty"`
		Hint             string                `json:"hint,omitempty"`
		HintLevel        int                   `json:"hintLevel,omitempty"`
		Solution         *scenario.Solution    `json:"solution,omitempty"`
		Explanation      string                `json:"explanation,omitempty"`
	}
	// StartRequest starts a challenge.
	StartRequest struct {
		ID string `json:"id"`
	}
	// HintRequest asks for a hint level (0 = next).
	HintRequest struct {
		Level int `json:"level"`
	}
	// StopRequest ends a challenge.
	StopRequest struct {
		Restore bool `json:"restore"`
	}
	// ErrorResponse is returned for failures.
	ErrorResponse struct {
		Error string `json:"error"`
	}
)

type admin struct {
	info    Info
	cluster *sim.Cluster
	engine  *scenario.Engine
	persist func()
}

func (a *admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, brand.AdminPathPrefix)
	route := r.Method + " " + path
	var resp any
	var err error
	switch route {
	case "GET /info":
		resp = a.info
	case "GET /challenges":
		resp = a.catalog()
	case "GET /challenge":
		resp, err = a.status(false)
	case "POST /challenge/start":
		var req StartRequest
		if err = decode(r, &req); err == nil {
			if _, _, err = a.engine.Start(req.ID); err == nil {
				resp, err = a.status(false)
			}
		}
	case "POST /challenge/hint":
		var req HintRequest
		if err = decode(r, &req); err == nil {
			var lvl, total int
			var text string
			if lvl, total, text, err = a.engine.Hint(req.Level); err == nil {
				var cr *ChallengeResponse
				if cr, err = a.status(false); err == nil {
					cr.Hint, cr.HintLevel, cr.HintsTotal = text, lvl, total
					resp = cr
				}
			}
		}
	case "POST /challenge/solve":
		var sc *scenario.Scenario
		if _, sc, err = a.engine.Solve(); err == nil {
			var cr *ChallengeResponse
			if cr, err = a.status(false); err == nil {
				cr.Solution = &sc.Spec.Solution
				cr.Explanation = sc.Spec.Explanation
				resp = cr
			}
		}
	case "POST /challenge/check":
		resp, err = a.status(true)
	case "POST /challenge/reset":
		if _, err = a.engine.Reset(); err == nil {
			resp, err = a.status(false)
		}
	case "POST /challenge/stop":
		var req StopRequest
		if err = decode(r, &req); err == nil {
			var cr *ChallengeResponse
			if cr, err = a.status(false); err == nil {
				if _, err = a.engine.Stop(req.Restore); err == nil {
					resp = cr
				}
			}
		}
	case "POST /reset":
		err = a.cluster.Reset()
		resp = map[string]string{"status": "reset"}
	default:
		writeAdminError(w, http.StatusNotFound, errors.New("unknown admin endpoint "+route))
		return
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, scenario.ErrNoChallenge) {
			code = http.StatusNotFound
		} else if errors.Is(err, scenario.ErrActive) {
			code = http.StatusConflict
		}
		writeAdminError(w, code, err)
		return
	}
	if r.Method == http.MethodPost && a.persist != nil {
		a.persist()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func decode(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeAdminError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: err.Error()})
}

func (a *admin) catalog() *CatalogResponse {
	solved := map[string]bool{}
	for _, h := range a.engine.History() {
		if h.Solved {
			solved[h.ID] = true
		}
	}
	out := &CatalogResponse{Warnings: a.engine.Catalog.Warnings}
	for _, s := range a.engine.Catalog.List() {
		out.Scenarios = append(out.Scenarios, ScenarioSummary{
			ID: s.Metadata.ID, Title: s.Metadata.Title, Difficulty: s.Metadata.Difficulty,
			EstimatedMinutes: s.Metadata.EstimatedMinutes, Tags: s.Metadata.Tags, Summary: s.Spec.Summary,
			Source: s.Source, Solved: solved[s.Metadata.ID],
		})
	}
	if st, _, err := a.engine.Active(); err == nil {
		out.Active = st.ID
	}
	return out
}

func (a *admin) status(record bool) (*ChallengeResponse, error) {
	st, res, err := a.engine.Check(record)
	if err != nil {
		return nil, err
	}
	_, sc, err := a.engine.Active()
	if err != nil {
		return nil, err
	}
	return &ChallengeResponse{
		ID: st.ID, Title: st.Title, Difficulty: sc.Metadata.Difficulty, Description: sc.Spec.Description,
		Target: st.Target, StartedAt: st.StartedAt, HintsRevealed: st.HintsRevealed, HintsTotal: len(sc.Spec.Hints),
		SolutionRevealed: st.SolutionRevealed, Solved: st.Solved, SelfSolved: st.SelfSolved, SolvedAt: st.SolvedAt,
		Checks: st.Checks, Resets: st.Resets, Check: res,
	}, nil
}
