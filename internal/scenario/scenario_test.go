package scenario

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/itskills/kubedrill/internal/registry"
	"github.com/itskills/kubedrill/internal/sim"
)

func newEngine(t *testing.T) (*Engine, *sim.FakeClock) {
	t.Helper()
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	clk := sim.NewFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	return &Engine{Cluster: sim.New(sim.Options{Clock: clk, Seed: "t"}), Catalog: cat}, clk
}

func run(e *Engine, clk *sim.FakeClock, d time.Duration) {
	for i := time.Duration(0); i < d; i += time.Second {
		clk.Advance(time.Second)
		e.Cluster.Tick()
	}
}

func mustRes(kind string) *registry.Resource {
	r, _ := kindResource(kind)
	return r
}

func mutateDeploy(t *testing.T, e *Engine, ns, name string, fn func(d *appsv1.Deployment)) {
	t.Helper()
	_, err := e.Cluster.Mutate(mustRes("Deployment"), ns, name, "", false, func(o runtime.Object) (runtime.Object, error) {
		d := o.(*appsv1.Deployment)
		fn(d)
		d.ResourceVersion = ""
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinScenariosValid(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cat.List()); n < 6 {
		t.Fatalf("expected at least 6 builtin scenarios, got %d", n)
	}
}

func TestValidationErrorsAreReadable(t *testing.T) {
	_, err := Parse([]byte("apiVersion: itskills.github.io/v1alpha1\nkind: Scenario\nmetadata:\n  id: Bad_ID\n  difficulty: hard\nspec:\n  hints: []\n  sucess: []\n"), "bad.yaml")
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
	_, err = Parse([]byte("apiVersion: itskills.github.io/v1alpha1\nkind: Scenario\nmetadata:\n  id: Bad_ID\n  difficulty: hard\nspec:\n  hints: []\n"), "bad.yaml")
	for _, want := range []string{"metadata.id", "difficulty", "spec.hints", "spec.success", "spec.target or spec.setup"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q in %v", want, err)
		}
	}
}

// fixes repairs each builtin scenario through regular API mutations.
var fixes = map[string]func(t *testing.T, e *Engine, st *State){
	"image-pull-backoff": func(t *testing.T, e *Engine, st *State) {
		mutateDeploy(t, e, st.Target.Namespace, st.Target.Name, func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Image = "nginx:1.27"
		})
	},
	"crashloop-missing-env": func(t *testing.T, e *Engine, _ *State) {
		mutateDeploy(t, e, "default", "orders-api", func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "orders-db"}, Key: "url"}}}}
		})
	},
	"ingress-wrong-port": func(t *testing.T, e *Engine, _ *State) {
		_, err := e.Cluster.Mutate(mustRes("Ingress"), "default", "shop", "", false, func(o runtime.Object) (runtime.Object, error) {
			i := o.(*networkingv1.Ingress)
			i.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number = 80
			return i, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	},
	"service-selector": func(t *testing.T, e *Engine, _ *State) {
		_, err := e.Cluster.Mutate(mustRes("Service"), "default", "shop-frontend", "", false, func(o runtime.Object) (runtime.Object, error) {
			s := o.(*corev1.Service)
			s.Spec.Selector = map[string]string{"app": "shop-frontend"}
			return s, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	},
	"missing-configmap": func(t *testing.T, e *Engine, _ *State) {
		_, err := e.Cluster.Create(mustRes("ConfigMap"), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "reporting-config", Namespace: "default"},
			Data: map[string]string{"LOG_LEVEL": "info", "REPORT_SCHEDULE": "0 6 * * *"}}, false)
		if err != nil {
			t.Fatal(err)
		}
	},
	"readiness-probe": func(t *testing.T, e *Engine, _ *State) {
		mutateDeploy(t, e, "default", "catalog", func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path = "/ready"
			d.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Port = intstr.FromString("http")
		})
	},
	"pending-resources": func(t *testing.T, e *Engine, _ *State) {
		mutateDeploy(t, e, "default", "analytics", func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}
		})
	},
}

func TestBuiltinScenarioLifecycle(t *testing.T) {
	cat, _ := LoadCatalog()
	for _, s := range cat.List() {
		t.Run(s.Metadata.ID, func(t *testing.T) {
			fix, ok := fixes[s.Metadata.ID]
			if !ok {
				t.Skip("no automated fix for this scenario")
			}
			e, clk := newEngine(t)
			st, _, err := e.Start(s.Metadata.ID)
			if err != nil {
				t.Fatal(err)
			}
			if ns := st.Target.Namespace; strings.HasPrefix(ns, "kube-") || ns == "ingress-nginx" {
				t.Fatalf("fault injected into platform workload %s/%s", st.Target.Namespace, st.Target.Name)
			}
			run(e, clk, 60*time.Second)
			_, res, err := e.Check(true)
			if err != nil {
				t.Fatal(err)
			}
			if res.Passed {
				t.Fatalf("challenge passes before any fix: %+v", res)
			}
			// Hints are graded and capped.
			if lvl, total, text, err := e.Hint(0); err != nil || lvl != 1 || text == "" || total < 2 {
				t.Fatalf("hint 1: %d/%d %q %v", lvl, total, text, err)
			}
			if lvl, _, _, _ := e.Hint(0); lvl != 2 {
				t.Fatalf("expected next hint level 2, got %d", lvl)
			}
			if _, _, _, err := e.Hint(99); err == nil {
				t.Fatal("expected error for invalid hint level")
			}
			fix(t, e, st)
			run(e, clk, 90*time.Second)
			st2, res, err := e.Check(true)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Passed {
				t.Fatalf("challenge not solved after fix: %+v", res)
			}
			if !st2.Solved || !st2.SelfSolved {
				t.Fatalf("expected self-solved state: %+v", st2)
			}
			// Reset returns to the broken starting point.
			if _, err := e.Reset(); err != nil {
				t.Fatal(err)
			}
			run(e, clk, 30*time.Second)
			if _, res, _ := e.Check(false); res.Passed {
				t.Fatal("challenge passes right after reset")
			}
			if _, err := e.Stop(true); err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.Active(); err == nil {
				t.Fatal("challenge still active after stop")
			}
		})
	}
}

func TestSolveDoesNotCountAsSelfSolved(t *testing.T) {
	e, clk := newEngine(t)
	st, _, err := e.Start("missing-configmap")
	if err != nil {
		t.Fatal(err)
	}
	if _, sc, err := e.Solve(); err != nil || sc.Spec.Solution.RootCause == "" {
		t.Fatalf("solve: %v", err)
	}
	fixes["missing-configmap"](t, e, st)
	run(e, clk, 30*time.Second)
	st, res, _ := e.Check(true)
	if !res.Passed || !st.Solved || st.SelfSolved {
		t.Fatalf("expected solved but not self-solved: %+v %+v", st, res)
	}
}

func TestInjectIntoExistingDeployment(t *testing.T) {
	e, clk := newEngine(t)
	lbl := map[string]string{"app": "mine"}
	one := int32(1)
	_, err := e.Cluster.Create(mustRes("Deployment"), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}}}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	run(e, clk, 5*time.Second)
	st, sc, err := e.Start("image-pull-backoff")
	if err != nil {
		t.Fatal(err)
	}
	if st.Target.Mode != "existing" || st.Target.Name != "mine" {
		t.Fatalf("expected injection into existing deployment, got %+v", st.Target)
	}
	if !strings.Contains(sc.Spec.Description, `"mine"`) {
		t.Fatalf("placeholders not rendered: %s", sc.Spec.Description)
	}
	// Stop with restore brings back the original image.
	if _, err := e.Stop(true); err != nil {
		t.Fatal(err)
	}
	o, _ := e.Cluster.Store.Get(mustRes("Deployment").GVR, "default", "mine")
	if img := o.(*appsv1.Deployment).Spec.Template.Spec.Containers[0].Image; img != "nginx" {
		t.Fatalf("restore failed, image=%s", img)
	}
}
