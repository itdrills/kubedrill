package sim

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
)

// The simulated ingress controller imitates ingress-nginx as installed by
// kind: a Deployment on the control-plane node and the default IngressClass
// "nginx". No traffic is routed; the controller publishes the Ingress
// address and reports broken backends (events and controller logs).
const (
	IngressNamespace      = "ingress-nginx"
	IngressControllerName = "ingress-nginx-controller"
	IngressClassName      = "nginx"
	IngressController     = "k8s.io/ingress-nginx"
	ingressControllerImg  = "registry.k8s.io/ingress-nginx/controller:v1.12.1"
	defaultClassAnno      = "ingressclass.kubernetes.io/is-default-class"
	legacyClassAnno       = "kubernetes.io/ingress.class"
	addonMarker           = "addon.ingress-nginx"
	ingressEventSource    = "nginx-ingress-controller"
	// ingressProblemGrace delays backend warnings (see syncIngresses).
	ingressProblemGrace = 5 * time.Second
)

var (
	gvrIngresses       = networkingv1.SchemeGroupVersion.WithResource("ingresses")
	gvrIngressClasses  = networkingv1.SchemeGroupVersion.WithResource("ingressclasses")
	ingressSelectorLbl = map[string]string{
		"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/instance": "ingress-nginx", "app.kubernetes.io/component": "controller",
	}
)

// ensureIngressAddon installs ingress-nginx once per cluster. The marker
// keeps it deleted if the learner removes it, and lets clusters saved by
// older versions receive the addon after an upgrade.
func (c *Cluster) ensureIngressAddon() {
	if c.st.Extra[addonMarker] != "" {
		return
	}
	if c.st.Extra == nil {
		c.st.Extra = map[string]string{}
	}
	c.st.Extra[addonMarker] = "installed"
	// Clusters saved by older versions lack the node label kind sets.
	if o, err := c.Store.Get(gvrNodes, "", DefaultNodes()[0].Name); err == nil {
		if n := o.(*corev1.Node); n.Labels["ingress-ready"] != "true" {
			n.Labels["ingress-ready"] = "true"
			_, _ = c.Store.Update(gvrNodes, n)
		}
	}
	if _, err := c.Store.Get(gvrNamespaces, "", IngressNamespace); err != nil {
		_, _ = c.create(res("namespaces"), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: IngressNamespace,
			Labels: map[string]string{"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/instance": "ingress-nginx"}}}, false)
	}
	if _, err := c.Store.Get(gvrIngressClasses, "", IngressClassName); err != nil {
		_, _ = c.create(res("ingressclasses"), &networkingv1.IngressClass{
			ObjectMeta: metav1.ObjectMeta{Name: IngressClassName, Labels: map[string]string{"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/component": "controller"},
				Annotations: map[string]string{defaultClassAnno: "true"}},
			Spec: networkingv1.IngressClassSpec{Controller: IngressController},
		}, false)
	}
	cp := DefaultNodes()[0].Name
	_, _ = c.create(res("services"), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: IngressControllerName, Namespace: IngressNamespace, Labels: ingressSelectorLbl},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: ingressSelectorLbl, Ports: []corev1.ServicePort{
			{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
			{Name: "https", Port: 443, TargetPort: intstr.FromString("https"), Protocol: corev1.ProtocolTCP},
		}},
	}, false)
	_, _ = c.create(res("deployments"), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: IngressControllerName, Namespace: IngressNamespace, Labels: ingressSelectorLbl},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: ingressSelectorLbl},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: ingressSelectorLbl},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"ingress-ready": "true", "kubernetes.io/os": "linux"},
					Tolerations: []corev1.Toleration{
						{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule},
					},
					Containers: []corev1.Container{{
						Name: "controller", Image: ingressControllerImg,
						Args: []string{"/nginx-ingress-controller", "--election-id=ingress-nginx-leader", "--controller-class=" + IngressController,
							"--ingress-class=" + IngressClassName, "--publish-status-address=" + DefaultNodes()[0].IP, "--watch-ingress-without-class=true"},
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80, HostPort: 80}, {Name: "https", ContainerPort: 443, HostPort: 443}},
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("90Mi")}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(80)}}},
					}},
				},
			},
		},
	}, false)
	// Like the control-plane images, the controller image is pre-loaded.
	c.st.NodeImages[cp] = append(c.st.NodeImages[cp], ingressControllerImg)
}

// defaultIngressClass mirrors the DefaultIngressClass admission plugin: on
// create, an Ingress without class gets the class marked as default.
func (c *Cluster) defaultIngressClass(ing *networkingv1.Ingress) {
	if ing.Spec.IngressClassName != nil || ing.Annotations[legacyClassAnno] != "" {
		return
	}
	for _, ic := range listTyped[*networkingv1.IngressClass](c, gvrIngressClasses, "") {
		if ic.Annotations[defaultClassAnno] == "true" {
			ing.Spec.IngressClassName = ptr.To(ic.Name)
			return
		}
	}
}

func validateIngress(ing *networkingv1.Ingress) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")
	if ing.Spec.DefaultBackend == nil && len(ing.Spec.Rules) == 0 {
		errs = append(errs, field.Invalid(spec, ing.Spec, "either `defaultBackend` or `rules` must be specified"))
	}
	if ing.Spec.IngressClassName != nil {
		for _, msg := range validation.IsDNS1123Subdomain(*ing.Spec.IngressClassName) {
			errs = append(errs, field.Invalid(spec.Child("ingressClassName"), *ing.Spec.IngressClassName, msg))
		}
	}
	if ing.Spec.DefaultBackend != nil {
		errs = append(errs, validateIngressBackend(ing.Spec.DefaultBackend, spec.Child("defaultBackend"))...)
	}
	for i, r := range ing.Spec.Rules {
		rp := spec.Child("rules").Index(i)
		if r.Host != "" {
			host := strings.TrimPrefix(r.Host, "*.")
			for _, msg := range validation.IsDNS1123Subdomain(host) {
				errs = append(errs, field.Invalid(rp.Child("host"), r.Host, msg))
			}
			if len(validation.IsValidIP(rp.Child("host"), r.Host)) == 0 {
				errs = append(errs, field.Invalid(rp.Child("host"), r.Host, "must be a DNS name, not an IP address"))
			}
		}
		if r.HTTP == nil {
			continue
		}
		if len(r.HTTP.Paths) == 0 {
			errs = append(errs, field.Required(rp.Child("http", "paths"), ""))
		}
		for j, p := range r.HTTP.Paths {
			pp := rp.Child("http", "paths").Index(j)
			switch {
			case p.PathType == nil:
				errs = append(errs, field.Required(pp.Child("pathType"), "pathType must be specified"))
			case *p.PathType != networkingv1.PathTypeExact && *p.PathType != networkingv1.PathTypePrefix && *p.PathType != networkingv1.PathTypeImplementationSpecific:
				errs = append(errs, field.NotSupported(pp.Child("pathType"), *p.PathType, []string{"Exact", "ImplementationSpecific", "Prefix"}))
			}
			if p.PathType != nil && *p.PathType != networkingv1.PathTypeImplementationSpecific || p.Path != "" {
				if !strings.HasPrefix(p.Path, "/") {
					errs = append(errs, field.Invalid(pp.Child("path"), p.Path, "must be an absolute path"))
				}
			}
			errs = append(errs, validateIngressBackend(&p.Backend, pp.Child("backend"))...)
		}
	}
	for i, t := range ing.Spec.TLS {
		for j, h := range t.Hosts {
			for _, msg := range validation.IsDNS1123Subdomain(strings.TrimPrefix(h, "*.")) {
				errs = append(errs, field.Invalid(spec.Child("tls").Index(i).Child("hosts").Index(j), h, msg))
			}
		}
	}
	return errs
}

func validateIngressBackend(b *networkingv1.IngressBackend, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	switch {
	case b.Service != nil && b.Resource != nil:
		return append(errs, field.Invalid(p, "", "cannot set both resource and service backends"))
	case b.Service == nil && b.Resource == nil:
		return append(errs, field.Invalid(p, "", "resource or service backend is required"))
	case b.Resource != nil:
		return errs
	}
	sp := p.Child("service")
	if b.Service.Name == "" {
		errs = append(errs, field.Required(sp.Child("name"), ""))
	} else {
		for _, msg := range validation.IsDNS1035Label(b.Service.Name) {
			errs = append(errs, field.Invalid(sp.Child("name"), b.Service.Name, msg))
		}
	}
	port := b.Service.Port
	switch {
	case port.Name != "" && port.Number != 0:
		errs = append(errs, field.Invalid(sp.Child("port"), port, "cannot set both port name & port number"))
	case port.Name != "":
		for _, msg := range validation.IsValidPortName(port.Name) {
			errs = append(errs, field.Invalid(sp.Child("port", "name"), port.Name, msg))
		}
	case port.Number != 0:
		for _, msg := range validation.IsValidPortNum(int(port.Number)) {
			errs = append(errs, field.Invalid(sp.Child("port", "number"), port.Number, msg))
		}
	default:
		errs = append(errs, field.Required(sp.Child("port"), "port name or number is required"))
	}
	return errs
}

func validateIngressClass(ic *networkingv1.IngressClass, old *networkingv1.IngressClass) field.ErrorList {
	var errs field.ErrorList
	p := field.NewPath("spec", "controller")
	switch {
	case ic.Spec.Controller == "":
		errs = append(errs, field.Required(p, ""))
	case len(ic.Spec.Controller) > 250:
		errs = append(errs, field.TooLong(p, "", 250))
	case !strings.Contains(ic.Spec.Controller, "/"):
		errs = append(errs, field.Invalid(p, ic.Spec.Controller, "must be a domain-prefixed path (such as \"acme.io/foo\")"))
	}
	if old != nil && old.Spec.Controller != ic.Spec.Controller {
		errs = append(errs, field.Invalid(p, ic.Spec.Controller, "field is immutable"))
	}
	return errs
}

// ingressBackends lists every service backend of an Ingress.
func ingressBackends(ing *networkingv1.Ingress) []*networkingv1.IngressServiceBackend {
	var out []*networkingv1.IngressServiceBackend
	if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
		out = append(out, b.Service)
	}
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for i := range r.HTTP.Paths {
			if s := r.HTTP.Paths[i].Backend.Service; s != nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// BackendProblems explains why backends of an Ingress cannot serve traffic
// (missing service, unknown port, no ready endpoints). It is shared by the
// controller and the challenge checks.
func BackendProblems(get func(kind, ns, name string) (any, bool), ing *networkingv1.Ingress) []string {
	seen := map[string]bool{}
	var problems []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			problems = append(problems, s)
		}
	}
	for _, b := range ingressBackends(ing) {
		key := ing.Namespace + "/" + b.Name
		o, ok := get("Service", ing.Namespace, b.Name)
		if !ok {
			add(fmt.Sprintf("Service %q not found", key))
			continue
		}
		svc := o.(*corev1.Service)
		var match *corev1.ServicePort
		for i := range svc.Spec.Ports {
			sp := &svc.Spec.Ports[i]
			if (b.Port.Number != 0 && sp.Port == b.Port.Number) || (b.Port.Name != "" && sp.Name == b.Port.Name) {
				match = sp
			}
		}
		if match == nil {
			port := b.Port.Name
			if port == "" {
				port = fmt.Sprint(b.Port.Number)
			}
			add(fmt.Sprintf("Service %q does not have any port %s (ports: %s)", key, port, servicePortList(svc)))
			continue
		}
		ready := 0
		if eo, ok := get("Endpoints", ing.Namespace, b.Name); ok {
			for _, ss := range eo.(*corev1.Endpoints).Subsets {
				ready += len(ss.Addresses)
			}
		}
		if ready == 0 {
			add(fmt.Sprintf("Service %q does not have any active Endpoint", key))
		}
	}
	sort.Strings(problems)
	return problems
}

func servicePortList(svc *corev1.Service) string {
	var ps []string
	for _, p := range svc.Spec.Ports {
		s := fmt.Sprint(p.Port)
		if p.Name != "" {
			s = p.Name + "=" + s
		}
		ps = append(ps, s)
	}
	if len(ps) == 0 {
		return "none"
	}
	return strings.Join(ps, ", ")
}

func (c *Cluster) lookupForIngress(kind, ns, name string) (any, bool) {
	gvr := gvrServices
	if kind == "Endpoints" {
		gvr = gvrEndpoints
	}
	o, err := c.Store.Get(gvr, ns, name)
	return o, err == nil
}

// ingressControllerReady reports whether the simulated controller runs.
func (c *Cluster) ingressControllerReady() bool {
	o, err := c.Store.Get(gvrDeployments, IngressNamespace, IngressControllerName)
	if err != nil {
		return false
	}
	return o.(*appsv1.Deployment).Status.AvailableReplicas > 0
}

// servedByController reports whether ingress-nginx is responsible for ing.
func (c *Cluster) servedByController(ing *networkingv1.Ingress) bool {
	class := ""
	if ing.Spec.IngressClassName != nil {
		class = *ing.Spec.IngressClassName
	} else if a := ing.Annotations[legacyClassAnno]; a != "" {
		return a == IngressClassName
	} else {
		return true // --watch-ingress-without-class=true
	}
	o, err := c.Store.Get(gvrIngressClasses, "", class)
	return err == nil && o.(*networkingv1.IngressClass).Spec.Controller == IngressController
}

func (c *Cluster) syncIngresses() {
	if c.st.Extra == nil {
		c.st.Extra = map[string]string{}
	}
	ready := c.ingressControllerReady()
	for _, ing := range listTyped[*networkingv1.Ingress](c, gvrIngresses, "") {
		if !ready || !c.servedByController(ing) {
			continue
		}
		key := "ingress/" + string(ing.UID)
		if c.st.Extra[key+"/gen"] != fmt.Sprint(ing.Generation) {
			c.st.Extra[key+"/gen"] = fmt.Sprint(ing.Generation)
			c.recordEvent(ing, "Ingress", corev1.EventTypeNormal, "Sync", "Scheduled for sync", ingressEventSource, "")
			c.controllerLog(fmt.Sprintf("I event.go:377] \"Event occurred\" object=\"%s/%s\" kind=\"Ingress\" apiVersion=\"networking.k8s.io/v1\" type=\"Normal\" reason=\"Sync\" message=\"Scheduled for sync\"", ing.Namespace, ing.Name))
		}
		// Problems are reported once they persisted for a grace period, so a
		// freshly created Deployment whose pods are still starting does not
		// leave a misleading warning behind.
		problems := BackendProblems(c.lookupForIngress, ing)
		fp := strings.Join(problems, "|")
		now := c.Clock.Now()
		if c.st.Extra[key+"/seen"] != fp {
			c.st.Extra[key+"/seen"] = fp
			c.st.Extra[key+"/since"] = now.Format(time.RFC3339Nano)
		}
		since, _ := time.Parse(time.RFC3339Nano, c.st.Extra[key+"/since"])
		if c.st.Extra[key+"/problems"] != fp && now.Sub(since) >= ingressProblemGrace {
			c.st.Extra[key+"/problems"] = fp
			for _, p := range problems {
				reason := "BackendNotReady"
				switch {
				case strings.Contains(p, "not found"):
					reason = "ServiceNotFound"
				case strings.Contains(p, "does not have any port"):
					reason = "ServicePortNotFound"
				case strings.Contains(p, "active Endpoint"):
					reason = "NoActiveEndpoints"
				}
				c.recordEvent(ing, "Ingress", corev1.EventTypeWarning, reason, p, ingressEventSource, "")
				c.controllerLog(fmt.Sprintf("W controller.go:1130] Error obtaining Endpoints for Ingress %s/%s: %s", ing.Namespace, ing.Name, p))
			}
		}
		addr := DefaultNodes()[0].IP
		if lb := ing.Status.LoadBalancer.Ingress; len(lb) != 1 || lb[0].IP != addr {
			ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: addr}}
			_, _ = c.Store.Update(gvrIngresses, ing)
			c.controllerLog(fmt.Sprintf("I status.go:304] \"updating Ingress status\" namespace=\"%s\" ingress=\"%s\" currentValue=null newValue=[{\"ip\":\"%s\"}]", ing.Namespace, ing.Name, addr))
		}
	}
	// Forget bookkeeping of deleted ingresses.
	for k := range c.st.Extra {
		if !strings.HasPrefix(k, "ingress/") {
			continue
		}
		uid := strings.SplitN(strings.TrimPrefix(k, "ingress/"), "/", 2)[0]
		found := false
		for _, ing := range listTyped[*networkingv1.Ingress](c, gvrIngresses, "") {
			if string(ing.UID) == uid {
				found = true
				break
			}
		}
		if !found {
			delete(c.st.Extra, k)
		}
	}
}

// controllerLog appends a line to the logs of running controller pods, so
// `kubectl logs -n ingress-nginx deploy/ingress-nginx-controller` shows what
// the controller did.
func (c *Cluster) controllerLog(line string) {
	ts := c.Clock.Now().UTC().Format("0102 15:04:05.000000")
	for _, p := range listTyped[*corev1.Pod](c, gvrPods, IngressNamespace) {
		rt := c.st.Pods[string(p.UID)]
		if rt == nil {
			continue
		}
		for _, ct := range rt.Containers {
			if ct.Phase == cRunning {
				ct.Logs = append(ct.Logs, line[:1]+ts+"       7 "+line[2:])
				if n := len(ct.Logs); n > 500 {
					ct.Logs = ct.Logs[n-500:]
				}
			}
		}
	}
}
