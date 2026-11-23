// Package scenario implements declarative troubleshooting challenges:
// loading and strict validation of scenario YAML files, fault injection into
// the simulated cluster, graded hints, end-state checks and resets.
package scenario

import (
	"encoding/json"
	"time"
)

// Scenario is the declarative description of one challenge.
type Scenario struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`

	// Source is the file the scenario was loaded from (not part of YAML).
	Source string `json:"-"`
	// raw is the original document, re-rendered with placeholders on start.
	raw []byte
}

// Metadata identifies and classifies a scenario.
type Metadata struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Difficulty       string   `json:"difficulty"`
	EstimatedMinutes int      `json:"estimatedMinutes,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Version          string   `json:"version,omitempty"`
	Author           string   `json:"author,omitempty"`
}

// Spec contains the behaviour of a scenario.
type Spec struct {
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Namespace   string      `json:"namespace,omitempty"`
	Target      *Target     `json:"target,omitempty"`
	Setup       string      `json:"setup,omitempty"`
	Inject      *Inject     `json:"inject,omitempty"`
	Hints       []string    `json:"hints"`
	Success     []Criterion `json:"success"`
	Solution    Solution    `json:"solution"`
	Explanation string      `json:"explanation,omitempty"`
	Reset       *Reset      `json:"reset,omitempty"`
}

// Target selects an existing object to inject the fault into. If no
// matching object exists, the setup manifests are created instead.
type Target struct {
	Kind          string   `json:"kind"`
	Namespaces    []string `json:"namespaces,omitempty"`
	LabelSelector string   `json:"labelSelector,omitempty"`
}

// Inject describes the fault as RFC 6902 JSON patch operations applied to
// the target object.
type Inject struct {
	// Object names the setup object to patch when setup is used. Defaults to
	// the first setup object of the target kind.
	Object *ObjectRef        `json:"object,omitempty"`
	Patch  []json.RawMessage `json:"patch"`
}

// ObjectRef references an object by kind/name/namespace.
type ObjectRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Criterion is one success condition evaluated against the cluster state.
type Criterion struct {
	Type        string  `json:"type"`
	Description string  `json:"description,omitempty"`
	Kind        string  `json:"kind,omitempty"`
	Name        string  `json:"name,omitempty"`
	Namespace   string  `json:"namespace,omitempty"`
	Selector    string  `json:"selector,omitempty"`
	Min         int     `json:"min,omitempty"`
	Path        string  `json:"path,omitempty"`
	Equals      *string `json:"equals,omitempty"`
	NotEquals   *string `json:"notEquals,omitempty"`
	Matches     string  `json:"matches,omitempty"`
}

// Solution explains the fault after the learner asks for it.
type Solution struct {
	Diagnosis string   `json:"diagnosis"`
	RootCause string   `json:"rootCause"`
	Fix       string   `json:"fix"`
	Commands  []string `json:"commands,omitempty"`
}

// Reset configures how `challenge reset` restores the starting point.
type Reset struct {
	Strategy string `json:"strategy"`
}

// TargetInfo is the object a running challenge is about.
type TargetInfo struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Mode is "existing" (fault injected into a learner's object) or "setup".
	Mode string `json:"mode"`
}

// State is the persisted state of the active challenge.
type State struct {
	ID               string     `json:"id"`
	Title            string     `json:"title"`
	StartedAt        time.Time  `json:"startedAt"`
	Target           TargetInfo `json:"target"`
	HintsRevealed    int        `json:"hintsRevealed"`
	SolutionRevealed bool       `json:"solutionRevealed"`
	Solved           bool       `json:"solved"`
	SolvedAt         *time.Time `json:"solvedAt,omitempty"`
	SelfSolved       bool       `json:"selfSolved"`
	Checks           int        `json:"checks"`
	Resets           int        `json:"resets"`
	Before           []byte     `json:"before"` // snapshot before injection
	Start            []byte     `json:"start"`  // snapshot right after injection
}

// CheckResult is the outcome of evaluating the success criteria.
type CheckResult struct {
	Passed   bool              `json:"passed"`
	Criteria []CriterionResult `json:"criteria"`
}

// CriterionResult is the outcome of one criterion.
type CriterionResult struct {
	Description string `json:"description"`
	Passed      bool   `json:"passed"`
	Detail      string `json:"detail,omitempty"`
}
