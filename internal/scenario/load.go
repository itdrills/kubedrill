package scenario

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	"github.com/itskills/kubedrill/internal/brand"
	"github.com/itskills/kubedrill/internal/registry"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

var idRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Difficulties allowed in metadata.difficulty.
var Difficulties = []string{"beginner", "intermediate", "advanced"}

// CriterionTypes lists supported success criterion types.
var CriterionTypes = map[string]string{
	"deploymentAvailable": "a Deployment has all desired replicas updated and available (requires name)",
	"podsReady":           "at least `min` pods matching `selector` are Ready and none matching are unready",
	"serviceHasEndpoints": "a Service has at least `min` ready endpoints (requires name)",
	"ingressReady":        "an Ingress has an address and every backend resolves to a Service port with ready endpoints (requires name)",
	"exists":              "an object exists (requires kind, name)",
	"absent":              "an object does not exist (requires kind, name)",
	"field":               "a field (JSON pointer `path`) of an object equals/notEquals/matches a value",
}

// ValidationError collects all problems of one scenario file.
type ValidationError struct {
	Source   string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: invalid scenario:\n  - %s", e.Source, strings.Join(e.Problems, "\n  - "))
}

// Parse decodes and validates one scenario document.
func Parse(data []byte, source string) (*Scenario, error) {
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return nil, &ValidationError{Source: source, Problems: []string{cleanYAMLError(err)}}
	}
	s.Source = source
	s.raw = data
	if probs := Validate(&s); len(probs) > 0 {
		return nil, &ValidationError{Source: source, Problems: probs}
	}
	return &s, nil
}

func cleanYAMLError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "error unmarshaling JSON: while decoding JSON: ")
	if strings.Contains(msg, "unknown field") {
		return msg + " (check spelling and indentation; see docs/scenarios.md)"
	}
	return msg
}

// Validate returns human readable problems; empty means valid.
func Validate(s *Scenario) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	if s.APIVersion != brand.ScenarioAPIVersion {
		add("apiVersion must be %q (got %q)", brand.ScenarioAPIVersion, s.APIVersion)
	}
	if s.Kind != "Scenario" {
		add("kind must be \"Scenario\" (got %q)", s.Kind)
	}
	if !idRe.MatchString(s.Metadata.ID) {
		add("metadata.id %q must be lowercase letters, digits and dashes (max 63 chars)", s.Metadata.ID)
	}
	if strings.TrimSpace(s.Metadata.Title) == "" {
		add("metadata.title is required")
	}
	okDiff := false
	for _, d := range Difficulties {
		if d == s.Metadata.Difficulty {
			okDiff = true
		}
	}
	if !okDiff {
		add("metadata.difficulty must be one of %s (got %q)", strings.Join(Difficulties, ", "), s.Metadata.Difficulty)
	}
	if strings.TrimSpace(s.Spec.Summary) == "" {
		add("spec.summary is required")
	}
	if strings.TrimSpace(s.Spec.Description) == "" {
		add("spec.description is required (the task shown to the learner)")
	}
	if s.Spec.Target == nil && strings.TrimSpace(s.Spec.Setup) == "" {
		add("either spec.target or spec.setup is required")
	}
	if s.Spec.Target != nil {
		if _, ok := kindResource(s.Spec.Target.Kind); !ok {
			add("spec.target.kind %q is not a supported resource kind", s.Spec.Target.Kind)
		}
		if s.Spec.Target.LabelSelector != "" {
			if _, err := labels.Parse(s.Spec.Target.LabelSelector); err != nil {
				add("spec.target.labelSelector: %v", err)
			}
		}
		if strings.TrimSpace(s.Spec.Setup) == "" {
			add("spec.setup is required as fallback when no object matches spec.target")
		}
	}
	var setupKinds []ObjectRef
	for i, doc := range registry.SplitYAML([]byte(s.Spec.Setup)) {
		r, obj, err := registry.Decode([]byte(stripPlaceholders(string(doc))))
		if err != nil {
			add("spec.setup document %d: %v", i+1, err)
			continue
		}
		m := metaOf(obj)
		if m.GetName() == "" {
			add("spec.setup document %d (%s): metadata.name is required", i+1, r.Kind)
		}
		setupKinds = append(setupKinds, ObjectRef{Kind: r.Kind, Name: m.GetName(), Namespace: m.GetNamespace()})
	}
	if s.Spec.Inject != nil {
		if len(s.Spec.Inject.Patch) == 0 {
			add("spec.inject.patch must contain at least one JSON patch operation")
		}
		b, _ := json.Marshal(s.Spec.Inject.Patch)
		if _, err := jsonpatch.DecodePatch(b); err != nil {
			add("spec.inject.patch is not a valid JSON patch: %v", err)
		}
		for i, op := range s.Spec.Inject.Patch {
			var o struct{ Op, Path string }
			_ = json.Unmarshal(op, &o)
			switch o.Op {
			case "add", "remove", "replace", "copy", "move", "test":
			default:
				add("spec.inject.patch[%d].op %q is not a JSON patch operation", i, o.Op)
			}
			if !strings.HasPrefix(o.Path, "/") {
				add("spec.inject.patch[%d].path must be a JSON pointer starting with '/'", i)
			}
		}
		if ref := s.Spec.Inject.Object; ref != nil {
			found := false
			for _, k := range setupKinds {
				if k.Kind == ref.Kind && k.Name == ref.Name {
					found = true
				}
			}
			if !found {
				add("spec.inject.object %s/%s does not match any object in spec.setup", ref.Kind, ref.Name)
			}
		} else if s.Spec.Target == nil {
			add("spec.inject.object is required when spec.target is not set")
		}
	}
	if len(s.Spec.Hints) == 0 {
		add("spec.hints needs at least one hint")
	}
	for i, h := range s.Spec.Hints {
		if strings.TrimSpace(h) == "" {
			add("spec.hints[%d] is empty", i)
		}
	}
	if len(s.Spec.Success) == 0 {
		add("spec.success needs at least one criterion")
	}
	for i, c := range s.Spec.Success {
		f := fmt.Sprintf("spec.success[%d]", i)
		if _, ok := CriterionTypes[c.Type]; !ok {
			var types []string
			for t := range CriterionTypes {
				types = append(types, t)
			}
			sort.Strings(types)
			add("%s.type %q is unknown (supported: %s)", f, c.Type, strings.Join(types, ", "))
			continue
		}
		switch c.Type {
		case "deploymentAvailable", "serviceHasEndpoints", "ingressReady":
			if c.Name == "" {
				add("%s.name is required for %s", f, c.Type)
			}
		case "exists", "absent":
			if c.Kind == "" || c.Name == "" {
				add("%s: kind and name are required for %s", f, c.Type)
			}
		case "podsReady":
			if c.Selector == "" {
				add("%s.selector is required for podsReady", f)
			} else if _, err := labels.Parse(stripPlaceholders(c.Selector)); err != nil {
				add("%s.selector: %v", f, err)
			}
		case "field":
			if c.Kind == "" || c.Name == "" || !strings.HasPrefix(c.Path, "/") {
				add("%s: kind, name and a JSON pointer path (starting with '/') are required for field", f)
			}
			if c.Equals == nil && c.NotEquals == nil && c.Matches == "" {
				add("%s: one of equals, notEquals or matches is required", f)
			}
			if c.Matches != "" {
				if _, err := regexp.Compile(c.Matches); err != nil {
					add("%s.matches: %v", f, err)
				}
			}
		}
		if c.Kind != "" {
			if _, ok := kindResource(c.Kind); !ok {
				add("%s.kind %q is not a supported resource kind", f, c.Kind)
			}
		}
	}
	if strings.TrimSpace(s.Spec.Solution.Diagnosis) == "" || strings.TrimSpace(s.Spec.Solution.RootCause) == "" || strings.TrimSpace(s.Spec.Solution.Fix) == "" {
		add("spec.solution.diagnosis, rootCause and fix are required")
	}
	if s.Spec.Reset != nil && s.Spec.Reset.Strategy != "" && s.Spec.Reset.Strategy != "snapshot" {
		add("spec.reset.strategy %q is not supported (supported: snapshot)", s.Spec.Reset.Strategy)
	}
	return p
}

var placeholderRe = regexp.MustCompile(`\{\{\s*target\.(name|namespace|kind)\s*\}\}`)

// stripPlaceholders replaces placeholders with syntactically valid dummies
// for validation.
func stripPlaceholders(s string) string {
	return placeholderRe.ReplaceAllString(s, "placeholder")
}

func kindResource(kind string) (*registry.Resource, bool) {
	for _, r := range registry.All() {
		if strings.EqualFold(r.Kind, kind) {
			return r, true
		}
	}
	return nil, false
}

// Catalog is a set of loaded scenarios.
type Catalog struct {
	byID     map[string]*Scenario
	Warnings []string
}

// Get returns a scenario by id.
func (c *Catalog) Get(id string) (*Scenario, bool) {
	s, ok := c.byID[id]
	return s, ok
}

// List returns scenarios sorted by difficulty, then id.
func (c *Catalog) List() []*Scenario {
	out := make([]*Scenario, 0, len(c.byID))
	for _, s := range c.byID {
		out = append(out, s)
	}
	rank := map[string]int{"beginner": 0, "intermediate": 1, "advanced": 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Metadata.Difficulty] != rank[out[j].Metadata.Difficulty] {
			return rank[out[i].Metadata.Difficulty] < rank[out[j].Metadata.Difficulty]
		}
		return out[i].Metadata.ID < out[j].Metadata.ID
	})
	return out
}

// LoadCatalog loads the built-in scenarios plus all *.yaml/*.yml files in
// the given directories. Invalid custom files produce warnings, not
// failures, so one broken file does not disable all challenges.
func LoadCatalog(dirs ...string) (*Catalog, error) {
	c := &Catalog{byID: map[string]*Scenario{}}
	err := fs.WalkDir(builtinFS, "builtin", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := builtinFS.ReadFile(path)
		if err != nil {
			return err
		}
		s, err := Parse(data, "builtin:"+filepath.Base(path))
		if err != nil {
			return err
		}
		c.byID[s.Metadata.ID] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		files, err := FindFiles(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			c.Warnings = append(c.Warnings, err.Error())
			continue
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				c.Warnings = append(c.Warnings, err.Error())
				continue
			}
			s, err := Parse(data, f)
			if err != nil {
				c.Warnings = append(c.Warnings, err.Error())
				continue
			}
			if old, ok := c.byID[s.Metadata.ID]; ok {
				c.Warnings = append(c.Warnings, fmt.Sprintf("%s overrides scenario %q from %s", f, s.Metadata.ID, old.Source))
			}
			c.byID[s.Metadata.ID] = s
		}
	}
	return c, nil
}

// FindFiles returns scenario files below path (or path itself).
func FindFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var files []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}
