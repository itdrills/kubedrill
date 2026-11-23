package sim

import (
	"fmt"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	"github.com/itskills/kubedrill/internal/registry"
)

// setDefaults applies the subset of API server defaulting relevant for the
// simulation and for realistic `kubectl get -o yaml` output.
func (c *Cluster) setDefaults(res *registry.Resource, obj runtime.Object) {
	switch o := obj.(type) {
	case *corev1.Pod:
		defaultPodSpec(&o.Spec)
		addDefaultTolerations(&o.Spec)
		if o.Status.Phase == "" {
			o.Status.Phase = corev1.PodPending
		}
		if o.Status.QOSClass == "" {
			o.Status.QOSClass = qosClass(&o.Spec)
		}
	case *appsv1.Deployment:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To[int32](1)
		}
		if o.Spec.Strategy.Type == "" {
			o.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		}
		if o.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType {
			if o.Spec.Strategy.RollingUpdate == nil {
				o.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{}
			}
			if o.Spec.Strategy.RollingUpdate.MaxSurge == nil {
				v := intstr.FromString("25%")
				o.Spec.Strategy.RollingUpdate.MaxSurge = &v
			}
			if o.Spec.Strategy.RollingUpdate.MaxUnavailable == nil {
				v := intstr.FromString("25%")
				o.Spec.Strategy.RollingUpdate.MaxUnavailable = &v
			}
		}
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To[int32](10)
		}
		if o.Spec.ProgressDeadlineSeconds == nil {
			o.Spec.ProgressDeadlineSeconds = ptr.To[int32](600)
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *appsv1.ReplicaSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To[int32](1)
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *corev1.Service:
		if o.Spec.Type == "" {
			o.Spec.Type = corev1.ServiceTypeClusterIP
		}
		if o.Spec.SessionAffinity == "" {
			o.Spec.SessionAffinity = corev1.ServiceAffinityNone
		}
		if o.Spec.InternalTrafficPolicy == nil {
			p := corev1.ServiceInternalTrafficPolicyCluster
			o.Spec.InternalTrafficPolicy = &p
		}
		for i := range o.Spec.Ports {
			p := &o.Spec.Ports[i]
			if p.Protocol == "" {
				p.Protocol = corev1.ProtocolTCP
			}
			if p.TargetPort.Type == intstr.Int && p.TargetPort.IntVal == 0 && p.TargetPort.StrVal == "" {
				p.TargetPort = intstr.FromInt32(p.Port)
			}
		}
		if (o.Spec.Type == corev1.ServiceTypeNodePort || o.Spec.Type == corev1.ServiceTypeLoadBalancer) && o.Spec.ExternalTrafficPolicy == "" {
			o.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		}
	case *corev1.Namespace:
		if o.Labels == nil {
			o.Labels = map[string]string{}
		}
		o.Labels["kubernetes.io/metadata.name"] = o.Name
		if len(o.Spec.Finalizers) == 0 {
			o.Spec.Finalizers = []corev1.FinalizerName{corev1.FinalizerKubernetes}
		}
		o.Status.Phase = corev1.NamespaceActive
	case *corev1.Secret:
		if o.Type == "" {
			o.Type = corev1.SecretTypeOpaque
		}
		for k, v := range o.StringData {
			if o.Data == nil {
				o.Data = map[string][]byte{}
			}
			o.Data[k] = []byte(v)
		}
		o.StringData = nil
	}
}

func defaultPodSpec(s *corev1.PodSpec) {
	if s.RestartPolicy == "" {
		s.RestartPolicy = corev1.RestartPolicyAlways
	}
	if s.DNSPolicy == "" {
		s.DNSPolicy = corev1.DNSClusterFirst
	}
	if s.SchedulerName == "" {
		s.SchedulerName = corev1.DefaultSchedulerName
	}
	if s.TerminationGracePeriodSeconds == nil {
		s.TerminationGracePeriodSeconds = ptr.To[int64](30)
	}
	if s.SecurityContext == nil {
		s.SecurityContext = &corev1.PodSecurityContext{}
	}
	if s.ServiceAccountName == "" {
		s.ServiceAccountName = "default"
	}
	for i := range s.Containers {
		defaultContainer(&s.Containers[i])
	}
	for i := range s.InitContainers {
		defaultContainer(&s.InitContainers[i])
	}
	for i := range s.Volumes {
		v := &s.Volumes[i]
		if v.ConfigMap != nil && v.ConfigMap.DefaultMode == nil {
			v.ConfigMap.DefaultMode = ptr.To[int32](0o644)
		}
		if v.Secret != nil && v.Secret.DefaultMode == nil {
			v.Secret.DefaultMode = ptr.To[int32](0o644)
		}
	}
}

func defaultContainer(ct *corev1.Container) {
	if ct.TerminationMessagePath == "" {
		ct.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if ct.TerminationMessagePolicy == "" {
		ct.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	if ct.ImagePullPolicy == "" {
		ref := ParseImage(ct.Image)
		if ref.Tag == "latest" && ref.Digest == "" {
			ct.ImagePullPolicy = corev1.PullAlways
		} else {
			ct.ImagePullPolicy = corev1.PullIfNotPresent
		}
	}
	for i := range ct.Ports {
		if ct.Ports[i].Protocol == "" {
			ct.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for _, p := range []*corev1.Probe{ct.ReadinessProbe, ct.LivenessProbe, ct.StartupProbe} {
		if p == nil {
			continue
		}
		if p.TimeoutSeconds == 0 {
			p.TimeoutSeconds = 1
		}
		if p.PeriodSeconds == 0 {
			p.PeriodSeconds = 10
		}
		if p.SuccessThreshold == 0 {
			p.SuccessThreshold = 1
		}
		if p.FailureThreshold == 0 {
			p.FailureThreshold = 3
		}
		if p.HTTPGet != nil {
			if p.HTTPGet.Scheme == "" {
				p.HTTPGet.Scheme = corev1.URISchemeHTTP
			}
			if p.HTTPGet.Path == "" {
				p.HTTPGet.Path = "/"
			}
		}
	}
}

func qosClass(s *corev1.PodSpec) corev1.PodQOSClass {
	anyReq, allGuaranteed := false, true
	for _, ct := range s.Containers {
		if len(ct.Resources.Requests) > 0 || len(ct.Resources.Limits) > 0 {
			anyReq = true
		}
		for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			l, okL := ct.Resources.Limits[r]
			q, okQ := ct.Resources.Requests[r]
			if !okL || (okQ && q.Cmp(l) != 0) {
				allGuaranteed = false
			}
		}
	}
	switch {
	case !anyReq:
		return corev1.PodQOSBestEffort
	case allGuaranteed:
		return corev1.PodQOSGuaranteed
	default:
		return corev1.PodQOSBurstable
	}
}

// validate implements a pragmatic subset of API validation with error
// messages formatted like the real API server.
func (c *Cluster) validate(res *registry.Resource, obj, old runtime.Object) error {
	m, _ := meta.Accessor(obj)
	var errs field.ErrorList
	if m.GetName() == "" {
		errs = append(errs, field.Required(field.NewPath("metadata", "name"), "name or generateName is required"))
	} else {
		var msgs []string
		switch res.Kind {
		case "Namespace", "Service":
			msgs = validation.IsDNS1123Label(m.GetName())
		case "Event":
		default:
			msgs = validation.IsDNS1123Subdomain(m.GetName())
		}
		for _, msg := range msgs {
			errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), m.GetName(), msg))
		}
	}
	for k, v := range m.GetLabels() {
		for _, msg := range validation.IsQualifiedName(k) {
			errs = append(errs, field.Invalid(field.NewPath("metadata", "labels"), k, msg))
		}
		for _, msg := range validation.IsValidLabelValue(v) {
			errs = append(errs, field.Invalid(field.NewPath("metadata", "labels"), v, msg))
		}
	}
	switch o := obj.(type) {
	case *corev1.Pod:
		errs = append(errs, validatePodSpec(&o.Spec, field.NewPath("spec"))...)
		if oldPod, ok := old.(*corev1.Pod); ok {
			errs = append(errs, validatePodUpdate(o, oldPod)...)
		}
	case *appsv1.Deployment:
		errs = append(errs, validateSelectorTemplate(o.Spec.Selector, o.Spec.Template.Labels)...)
		errs = append(errs, validatePodSpec(&o.Spec.Template.Spec, field.NewPath("spec", "template", "spec"))...)
		if o.Spec.Replicas != nil && *o.Spec.Replicas < 0 {
			errs = append(errs, field.Invalid(field.NewPath("spec", "replicas"), *o.Spec.Replicas, "must be greater than or equal to 0"))
		}
		if od, ok := old.(*appsv1.Deployment); ok && !reflect.DeepEqual(od.Spec.Selector, o.Spec.Selector) {
			errs = append(errs, field.Invalid(field.NewPath("spec", "selector"), o.Spec.Selector, "field is immutable"))
		}
	case *appsv1.ReplicaSet:
		errs = append(errs, validateSelectorTemplate(o.Spec.Selector, o.Spec.Template.Labels)...)
		errs = append(errs, validatePodSpec(&o.Spec.Template.Spec, field.NewPath("spec", "template", "spec"))...)
	case *corev1.Service:
		if len(o.Spec.Ports) == 0 && o.Spec.ClusterIP != corev1.ClusterIPNone && o.Spec.Type != corev1.ServiceTypeExternalName {
			errs = append(errs, field.Required(field.NewPath("spec", "ports"), ""))
		}
		for i, p := range o.Spec.Ports {
			if p.Port < 1 || p.Port > 65535 {
				errs = append(errs, field.Invalid(field.NewPath("spec", "ports").Index(i).Child("port"), p.Port, "must be between 1 and 65535, inclusive"))
			}
			if len(o.Spec.Ports) > 1 && p.Name == "" {
				errs = append(errs, field.Required(field.NewPath("spec", "ports").Index(i).Child("name"), ""))
			}
		}
		if os, ok := old.(*corev1.Service); ok && os.Spec.ClusterIP != "" && o.Spec.ClusterIP != "" && os.Spec.ClusterIP != o.Spec.ClusterIP {
			errs = append(errs, field.Invalid(field.NewPath("spec", "clusterIPs").Index(0), o.Spec.ClusterIP, "may not change once set"))
		}
	case *networkingv1.Ingress:
		errs = append(errs, validateIngress(o)...)
	case *networkingv1.IngressClass:
		oldIC, _ := old.(*networkingv1.IngressClass)
		errs = append(errs, validateIngressClass(o, oldIC)...)
	case *corev1.ConfigMap:
		for k := range o.Data {
			for _, msg := range validation.IsConfigMapKey(k) {
				errs = append(errs, field.Invalid(field.NewPath("data").Key(k), k, msg))
			}
		}
	case *corev1.Secret:
		for k := range o.Data {
			for _, msg := range validation.IsConfigMapKey(k) {
				errs = append(errs, field.Invalid(field.NewPath("data").Key(k), k, msg))
			}
		}
	}
	if len(errs) > 0 {
		return apierrors.NewInvalid(res.GVK().GroupKind(), m.GetName(), errs)
	}
	return nil
}

func validateSelectorTemplate(sel *metav1.LabelSelector, labels map[string]string) field.ErrorList {
	var errs field.ErrorList
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return append(errs, field.Required(field.NewPath("spec", "selector"), ""))
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return append(errs, field.Invalid(field.NewPath("spec", "selector"), sel, err.Error()))
	}
	if !s.Matches(labelsSet(labels)) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "template", "metadata", "labels"), labels, "`selector` does not match template `labels`"))
	}
	return errs
}

func validatePodSpec(s *corev1.PodSpec, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(s.Containers) == 0 {
		errs = append(errs, field.Required(p.Child("containers"), ""))
	}
	names := map[string]bool{}
	for i, ct := range s.Containers {
		cp := p.Child("containers").Index(i)
		if ct.Name == "" {
			errs = append(errs, field.Required(cp.Child("name"), ""))
		} else {
			for _, msg := range validation.IsDNS1123Label(ct.Name) {
				errs = append(errs, field.Invalid(cp.Child("name"), ct.Name, msg))
			}
			if names[ct.Name] {
				errs = append(errs, field.Duplicate(cp.Child("name"), ct.Name))
			}
			names[ct.Name] = true
		}
		if strings.TrimSpace(ct.Image) == "" {
			errs = append(errs, field.Required(cp.Child("image"), ""))
		}
		for j, port := range ct.Ports {
			if port.ContainerPort < 1 || port.ContainerPort > 65535 {
				errs = append(errs, field.Invalid(cp.Child("ports").Index(j).Child("containerPort"), port.ContainerPort, "must be between 1 and 65535, inclusive"))
			}
		}
		for rn, q := range ct.Resources.Requests {
			if l, ok := ct.Resources.Limits[rn]; ok && q.Cmp(l) > 0 {
				errs = append(errs, field.Invalid(cp.Child("resources", "requests"), q.String(), fmt.Sprintf("must be less than or equal to %s limit of %s", rn, l.String())))
			}
		}
	}
	return errs
}

// validatePodUpdate mirrors the API server rule that most pod spec fields are
// immutable.
func validatePodUpdate(newPod, oldPod *corev1.Pod) field.ErrorList {
	a := oldPod.Spec.DeepCopy()
	b := newPod.Spec.DeepCopy()
	for i := range b.Containers {
		if i < len(a.Containers) {
			a.Containers[i].Image = b.Containers[i].Image
		}
	}
	for i := range b.InitContainers {
		if i < len(a.InitContainers) {
			a.InitContainers[i].Image = b.InitContainers[i].Image
		}
	}
	a.ActiveDeadlineSeconds = b.ActiveDeadlineSeconds
	a.Tolerations = b.Tolerations
	a.TerminationGracePeriodSeconds = b.TerminationGracePeriodSeconds
	if a.NodeName == "" {
		a.NodeName = b.NodeName
	}
	if reflect.DeepEqual(a, b) {
		return nil
	}
	return field.ErrorList{field.Forbidden(field.NewPath("spec"),
		"pod updates may not change fields other than `spec.containers[*].image`,`spec.initContainers[*].image`,`spec.activeDeadlineSeconds`,`spec.tolerations` (only additions to existing tolerations),`spec.terminationGracePeriodSeconds` (allow it to be set to 1 if it was previously negative)")}
}

// allocate assigns cluster IPs and node ports to services.
func (c *Cluster) allocate(res *registry.Resource, obj runtime.Object) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		if svc.Spec.ClusterIP == "" {
			n := c.st.NextSvcIP
			c.st.NextSvcIP++
			svc.Spec.ClusterIP = fmt.Sprintf("10.96.%d.%d", n/250, n%250+1)
		}
		svc.Spec.ClusterIPs = []string{svc.Spec.ClusterIP}
		if svc.Spec.ClusterIP != corev1.ClusterIPNone {
			fam := corev1.IPv4Protocol
			svc.Spec.IPFamilies = []corev1.IPFamily{fam}
			pol := corev1.IPFamilyPolicySingleStack
			svc.Spec.IPFamilyPolicy = &pol
		}
	}
	if svc.Spec.Type == corev1.ServiceTypeNodePort || svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		for i := range svc.Spec.Ports {
			if svc.Spec.Ports[i].NodePort == 0 {
				svc.Spec.Ports[i].NodePort = c.st.NextPort
				c.st.NextPort++
			}
		}
	}
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		// No cloud provider: the external IP stays <pending>, as on bare metal.
		svc.Status.LoadBalancer = corev1.LoadBalancerStatus{}
	}
}

// addDefaultTolerations mirrors the DefaultTolerationSeconds admission plugin.
func addDefaultTolerations(s *corev1.PodSpec) {
	for _, key := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
		found := false
		for _, t := range s.Tolerations {
			if t.Key == key || (t.Key == "" && t.Operator == corev1.TolerationOpExists) {
				found = true
			}
		}
		if !found {
			s.Tolerations = append(s.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](300)})
		}
	}
}
