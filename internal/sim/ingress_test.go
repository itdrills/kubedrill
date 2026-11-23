package sim

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func ingress(name, svc string, port int32) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: name + ".example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", PathType: ptr.To(networkingv1.PathTypePrefix),
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Number: port}}},
			}}}},
		}}},
	}
}

func ingressEvents(c *Cluster, name string) []string {
	var out []string
	for _, ev := range listTyped[*corev1.Event](c, corev1.SchemeGroupVersion.WithResource("events"), "default") {
		if ev.InvolvedObject.Kind == "Ingress" && ev.InvolvedObject.Name == name {
			out = append(out, ev.Type+" "+ev.Reason+": "+ev.Message)
		}
	}
	return out
}

func TestIngressControllerAddon(t *testing.T) {
	c, clk := newTestCluster(t)
	advance(c, clk, 15*time.Second)
	ctrl := pods(c, IngressNamespace, IngressControllerName)
	if len(ctrl) != 1 || ctrl[0].Status.Phase != corev1.PodRunning || ctrl[0].Spec.NodeName != DefaultNodes()[0].Name {
		t.Fatalf("controller pod should run on the control plane: %+v", ctrl)
	}
	if _, err := c.Store.Get(gvrIngressClasses, "", IngressClassName); err != nil {
		t.Fatal("default IngressClass missing")
	}
}

func TestIngressLifecycle(t *testing.T) {
	c, clk := newTestCluster(t)
	if _, err := c.Create(res("deployments"), deployment("web", "nginx:1.27", 2), false); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80)}}}}
	if _, err := c.Create(res("services"), svc, false); err != nil {
		t.Fatal(err)
	}
	// Wrong service port.
	out, err := c.Create(res("ingresses"), ingress("web", "web", 8080), false)
	if err != nil {
		t.Fatal(err)
	}
	if cls := out.(*networkingv1.Ingress).Spec.IngressClassName; cls == nil || *cls != IngressClassName {
		t.Fatalf("default ingress class not applied: %v", cls)
	}
	advance(c, clk, 15*time.Second)
	o, _ := c.Store.Get(gvrIngresses, "default", "web")
	ing := o.(*networkingv1.Ingress)
	if lb := ing.Status.LoadBalancer.Ingress; len(lb) != 1 || lb[0].IP != DefaultNodes()[0].IP {
		t.Fatalf("address not published: %+v", lb)
	}
	evs := strings.Join(ingressEvents(c, "web"), "\n")
	if !strings.Contains(evs, "Normal Sync: Scheduled for sync") || !strings.Contains(evs, `Warning ServicePortNotFound: Service "default/web" does not have any port 8080 (ports: 80)`) {
		t.Fatalf("unexpected events:\n%s", evs)
	}
	// Pods that were still starting when the Ingress appeared leave no warning.
	if strings.Contains(evs, "NoActiveEndpoints") {
		t.Fatalf("transient startup state was reported:\n%s", evs)
	}
	// The warning is reported once, not on every tick.
	advance(c, clk, 10*time.Second)
	if n := strings.Count(strings.Join(ingressEvents(c, "web"), "\n"), "ServicePortNotFound"); n != 1 {
		t.Fatalf("warning recorded %d times", n)
	}
	// Fix the port: no problems remain.
	_, err = c.Mutate(res("ingresses"), "default", "web", "", false, func(cur runtime.Object) (runtime.Object, error) {
		i := cur.(*networkingv1.Ingress)
		i.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number = 80
		return i, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	advance(c, clk, 3*time.Second)
	o, _ = c.Store.Get(gvrIngresses, "default", "web")
	if p := BackendProblems(c.lookupForIngress, o.(*networkingv1.Ingress)); len(p) != 0 {
		t.Fatalf("problems after fix: %v", p)
	}
	logs := ""
	for _, rt := range c.st.Pods {
		for _, ct := range rt.Containers {
			if ct.Name == "controller" {
				logs = strings.Join(ct.Logs, "\n")
			}
		}
	}
	if !strings.Contains(logs, "does not have any port 8080") {
		t.Fatalf("controller logs miss the backend error:\n%s", logs)
	}
}

func TestIngressValidation(t *testing.T) {
	c, _ := newTestCluster(t)
	bad := ingress("bad", "web", 80)
	bad.Spec.Rules[0].HTTP.Paths[0].PathType = nil
	bad.Spec.Rules[0].HTTP.Paths[0].Path = "api"
	_, err := c.Create(res("ingresses"), bad, false)
	if err == nil || !strings.Contains(err.Error(), "pathType must be specified") || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("want validation errors, got %v", err)
	}
	empty := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"}}
	if _, err := c.Create(res("ingresses"), empty, false); err == nil || !strings.Contains(err.Error(), "either `defaultBackend` or `rules` must be specified") {
		t.Fatalf("empty ingress: %v", err)
	}
	ic := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Spec: networkingv1.IngressClassSpec{Controller: "nodomain"}}
	if _, err := c.Create(res("ingressclasses"), ic, false); err == nil || !strings.Contains(err.Error(), "domain-prefixed") {
		t.Fatalf("ingressclass: %v", err)
	}
}

func TestIngressOfForeignClassIsIgnored(t *testing.T) {
	c, clk := newTestCluster(t)
	ing := ingress("other", "web", 80)
	ing.Spec.IngressClassName = ptr.To("traefik")
	if _, err := c.Create(res("ingresses"), ing, false); err != nil {
		t.Fatal(err)
	}
	advance(c, clk, 10*time.Second)
	o, _ := c.Store.Get(gvrIngresses, "default", "other")
	if lb := o.(*networkingv1.Ingress).Status.LoadBalancer.Ingress; len(lb) != 0 {
		t.Fatalf("ingress of unknown class must not get an address: %+v", lb)
	}
}
