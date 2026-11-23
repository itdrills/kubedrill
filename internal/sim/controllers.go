package sim

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	revisionAnnotation = "deployment.kubernetes.io/revision"
	hashLabel          = appsv1.DefaultDeploymentUniqueLabelKey
)

var gvrEndpointSlices = discoveryv1.SchemeGroupVersion.WithResource("endpointslices")

// reconcile runs all controllers until the store stops changing.
func (c *Cluster) reconcile() {
	for i := 0; i < 12; i++ {
		before := c.Store.ResourceVersion()
		c.syncNamespaces()
		c.syncDeployments()
		c.syncReplicaSets()
		c.syncScheduler()
		c.syncKubelet()
		c.syncReplicaSetStatus()
		c.syncDeploymentStatus()
		c.syncEndpoints()
		c.syncIngresses()
		if c.Store.ResourceVersion() == before {
			return
		}
	}
}

func isControlledBy(m metav1.Object, uid types.UID) bool {
	for _, r := range m.GetOwnerReferences() {
		if r.Controller != nil && *r.Controller && r.UID == uid {
			return true
		}
	}
	return false
}

func controllerRef(kind, name string, uid string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
}

// ---------------------------------------------------------------------------
// Namespaces: default service account and root CA config map.

func (c *Cluster) syncNamespaces() {
	for _, ns := range listTyped[*corev1.Namespace](c, gvrNamespaces, "") {
		if _, err := c.Store.Get(gvrSAs, ns.Name, "default"); err != nil {
			_, _ = c.create(res("serviceaccounts"), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns.Name}}, false)
		}
		if _, err := c.Store.Get(gvrConfigMaps, ns.Name, "kube-root-ca.crt"); err != nil {
			_, _ = c.create(res("configmaps"), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: ns.Name, Annotations: map[string]string{"kubernetes.io/description": "Contains a CA bundle that can be used to verify the kube-apiserver when using internal endpoints such as the internal service IP or kubernetes.default.svc. (simulated, not a real certificate)"}},
				Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\nU0lNVUxBVEVEIEJZIEtVQkVTSU0gLSBOT1QgQSBSRUFMIENFUlRJRklDQVRF\n-----END CERTIFICATE-----\n"},
			}, false)
		}
	}
}

// ---------------------------------------------------------------------------
// Deployments

func podTemplateHash(t *corev1.PodTemplateSpec) string {
	cp := t.DeepCopy()
	delete(cp.Labels, hashLabel)
	b, _ := json.Marshal(cp)
	return templateHash(b)
}

func revisionOf(m metav1.Object) int64 {
	v, _ := strconv.ParseInt(m.GetAnnotations()[revisionAnnotation], 10, 64)
	return v
}

func resolveFenceposts(surge, unavail *intstr.IntOrString, desired int32) (int32, int32) {
	s, _ := intstr.GetScaledValueFromIntOrPercent(intstr.ValueOrDefault(surge, intstr.FromInt32(0)), int(desired), true)
	u, _ := intstr.GetScaledValueFromIntOrPercent(intstr.ValueOrDefault(unavail, intstr.FromInt32(0)), int(desired), false)
	if s == 0 && u == 0 {
		u = 1
	}
	return int32(s), int32(u)
}

func (c *Cluster) ownedReplicaSets(d *appsv1.Deployment) []*appsv1.ReplicaSet {
	var out []*appsv1.ReplicaSet
	for _, rs := range listTyped[*appsv1.ReplicaSet](c, gvrReplicaSets, d.Namespace) {
		if isControlledBy(rs, d.UID) {
			out = append(out, rs)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreationTimestamp.Equal(&out[j].CreationTimestamp) {
			return out[i].Name < out[j].Name
		}
		return out[i].CreationTimestamp.Before(&out[j].CreationTimestamp)
	})
	return out
}

func (c *Cluster) scaleRS(d *appsv1.Deployment, rs *appsv1.ReplicaSet, to int32) {
	from := *rs.Spec.Replicas
	if from == to {
		return
	}
	rs.Spec.Replicas = ptr.To(to)
	if rs.Annotations == nil {
		rs.Annotations = map[string]string{}
	}
	rs.Annotations["deployment.kubernetes.io/desired-replicas"] = strconv.Itoa(int(*d.Spec.Replicas))
	if _, err := c.Store.Update(gvrReplicaSets, rs); err != nil {
		return
	}
	dir := "up"
	if to < from {
		dir = "down"
	}
	c.recordEvent(d, "Deployment", corev1.EventTypeNormal, "ScalingReplicaSet",
		fmt.Sprintf("Scaled %s replica set %s from %d to %d", dir, rs.Name, from, to), "deployment-controller", "")
}

func (c *Cluster) syncDeployments() {
	for _, d := range listTyped[*appsv1.Deployment](c, gvrDeployments, "") {
		c.syncDeployment(d)
	}
}

func (c *Cluster) syncDeployment(d *appsv1.Deployment) {
	rsList := c.ownedReplicaSets(d)
	hash := podTemplateHash(&d.Spec.Template)
	var newRS *appsv1.ReplicaSet
	var olds []*appsv1.ReplicaSet
	var maxRev int64
	for _, rs := range rsList {
		if r := revisionOf(rs); r > maxRev {
			maxRev = r
		}
		if rs.Labels[hashLabel] == hash {
			newRS = rs
		} else {
			olds = append(olds, rs)
		}
	}
	desired := *d.Spec.Replicas
	if d.Spec.Paused {
		return
	}
	if newRS == nil {
		tmpl := d.Spec.Template.DeepCopy()
		if tmpl.Labels == nil {
			tmpl.Labels = map[string]string{}
		}
		tmpl.Labels[hashLabel] = hash
		sel := d.Spec.Selector.DeepCopy()
		if sel.MatchLabels == nil {
			sel.MatchLabels = map[string]string{}
		}
		sel.MatchLabels[hashLabel] = hash
		lbls := map[string]string{}
		for k, v := range tmpl.Labels {
			lbls[k] = v
		}
		surge := int32(0)
		if d.Spec.Strategy.RollingUpdate != nil {
			surge, _ = resolveFenceposts(d.Spec.Strategy.RollingUpdate.MaxSurge, d.Spec.Strategy.RollingUpdate.MaxUnavailable, desired)
		}
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: d.Name + "-" + hash, Namespace: d.Namespace, Labels: lbls,
				Annotations: map[string]string{
					revisionAnnotation:                          strconv.FormatInt(maxRev+1, 10),
					"deployment.kubernetes.io/desired-replicas": strconv.Itoa(int(desired)),
					"deployment.kubernetes.io/max-replicas":     strconv.Itoa(int(desired + surge)),
				},
				OwnerReferences: []metav1.OwnerReference{controllerRef("Deployment", d.Name, string(d.UID))},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To[int32](0), Selector: sel, Template: *tmpl, MinReadySeconds: d.Spec.MinReadySeconds},
		}
		created, err := c.create(res("replicasets"), rs, false)
		if err != nil {
			return
		}
		newRS = created.(*appsv1.ReplicaSet)
	} else if revisionOf(newRS) < maxRev {
		// Rollback to an older template: it becomes the newest revision.
		newRS.Annotations[revisionAnnotation] = strconv.FormatInt(maxRev+1, 10)
		_, _ = c.Store.Update(gvrReplicaSets, newRS)
	}
	if d.Annotations[revisionAnnotation] != newRS.Annotations[revisionAnnotation] {
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[revisionAnnotation] = newRS.Annotations[revisionAnnotation]
		if updated, err := c.Store.Update(gvrDeployments, d); err == nil {
			d = updated.(*appsv1.Deployment)
		}
	}

	if d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		oldPods := 0
		for _, o := range olds {
			if *o.Spec.Replicas > 0 {
				c.scaleRS(d, o, 0)
			}
			oldPods += int(o.Status.Replicas)
		}
		if oldPods == 0 {
			c.scaleRS(d, newRS, desired)
		}
	} else {
		var mu *intstr.IntOrString
		var ms *intstr.IntOrString
		if d.Spec.Strategy.RollingUpdate != nil {
			ms, mu = d.Spec.Strategy.RollingUpdate.MaxSurge, d.Spec.Strategy.RollingUpdate.MaxUnavailable
		}
		surge, unavail := resolveFenceposts(ms, mu, desired)
		total := int32(0)
		for _, rs := range append([]*appsv1.ReplicaSet{newRS}, olds...) {
			total += *rs.Spec.Replicas
		}
		if *newRS.Spec.Replicas > desired {
			c.scaleRS(d, newRS, desired)
		} else if *newRS.Spec.Replicas < desired {
			if len(olds) == 0 || activeOld(olds) == 0 {
				c.scaleRS(d, newRS, desired)
			} else if allowed := desired + surge - total; allowed > 0 {
				c.scaleRS(d, newRS, min(desired, *newRS.Spec.Replicas+allowed))
			}
		}
		// Scale down old replica sets while respecting maxUnavailable.
		minAvail := desired - unavail
		total = *newRS.Spec.Replicas
		available := newRS.Status.AvailableReplicas
		for _, o := range olds {
			total += *o.Spec.Replicas
			available += o.Status.AvailableReplicas
		}
		newUnavailable := *newRS.Spec.Replicas - newRS.Status.AvailableReplicas
		maxDown := total - minAvail - newUnavailable
		// First remove unhealthy old replicas, they do not count as available.
		for _, o := range olds {
			if maxDown <= 0 {
				break
			}
			unhealthy := *o.Spec.Replicas - o.Status.AvailableReplicas
			if unhealthy <= 0 {
				continue
			}
			n := min(unhealthy, maxDown)
			c.scaleRS(d, o, *o.Spec.Replicas-n)
			maxDown -= n
			total -= n
		}
		canDown := available - minAvail
		for _, o := range olds {
			if canDown <= 0 {
				break
			}
			if *o.Spec.Replicas == 0 {
				continue
			}
			n := min(*o.Spec.Replicas, canDown)
			c.scaleRS(d, o, *o.Spec.Replicas-n)
			canDown -= n
		}
	}
	// Clean up history beyond revisionHistoryLimit.
	limit := int(ptr.Deref(d.Spec.RevisionHistoryLimit, 10))
	var idle []*appsv1.ReplicaSet
	for _, o := range olds {
		if *o.Spec.Replicas == 0 && o.Status.Replicas == 0 {
			idle = append(idle, o)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return revisionOf(idle[i]) < revisionOf(idle[j]) })
	for len(idle) > limit {
		_ = c.delete(res("replicasets"), idle[0], metav1.DeletePropagationBackground)
		idle = idle[1:]
	}
}

func activeOld(olds []*appsv1.ReplicaSet) int32 {
	var n int32
	for _, o := range olds {
		n += *o.Spec.Replicas + o.Status.Replicas
	}
	return n
}

func (c *Cluster) syncDeploymentStatus() {
	now := c.now()
	for _, d := range listTyped[*appsv1.Deployment](c, gvrDeployments, "") {
		rsList := c.ownedReplicaSets(d)
		hash := podTemplateHash(&d.Spec.Template)
		var st appsv1.DeploymentStatus
		st.ObservedGeneration = d.Generation
		var newRS *appsv1.ReplicaSet
		for _, rs := range rsList {
			st.Replicas += rs.Status.Replicas
			st.ReadyReplicas += rs.Status.ReadyReplicas
			st.AvailableReplicas += rs.Status.AvailableReplicas
			if rs.Labels[hashLabel] == hash {
				newRS = rs
				st.UpdatedReplicas = rs.Status.Replicas
			}
		}
		desired := *d.Spec.Replicas
		st.UnavailableReplicas = max(0, desired-st.AvailableReplicas)
		var unavail int32 = 0
		if d.Spec.Strategy.RollingUpdate != nil {
			_, unavail = resolveFenceposts(d.Spec.Strategy.RollingUpdate.MaxSurge, d.Spec.Strategy.RollingUpdate.MaxUnavailable, desired)
		}
		minAvail := desired - unavail
		find := func(t appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
			for i := range d.Status.Conditions {
				if d.Status.Conditions[i].Type == t {
					return &d.Status.Conditions[i]
				}
			}
			return nil
		}
		mk := func(t appsv1.DeploymentConditionType, ok bool, reason, msg string, progressed bool) appsv1.DeploymentCondition {
			s := corev1.ConditionFalse
			if ok {
				s = corev1.ConditionTrue
			}
			cond := appsv1.DeploymentCondition{Type: t, Status: s, Reason: reason, Message: msg, LastUpdateTime: now, LastTransitionTime: now}
			if old := find(t); old != nil {
				if old.Status == s {
					cond.LastTransitionTime = old.LastTransitionTime
				}
				if old.Status == s && old.Reason == reason && old.Message == msg && !progressed {
					cond.LastUpdateTime = old.LastUpdateTime
				}
			}
			return cond
		}
		avail := mk(appsv1.DeploymentAvailable, st.AvailableReplicas >= minAvail,
			map[bool]string{true: "MinimumReplicasAvailable", false: "MinimumReplicasUnavailable"}[st.AvailableReplicas >= minAvail],
			map[bool]string{true: "Deployment has minimum availability.", false: "Deployment does not have minimum availability."}[st.AvailableReplicas >= minAvail], false)
		rsName := ""
		if newRS != nil {
			rsName = newRS.Name
		}
		complete := st.UpdatedReplicas == desired && st.Replicas == desired && st.AvailableReplicas == desired
		progressed := st.Replicas != d.Status.Replicas || st.UpdatedReplicas != d.Status.UpdatedReplicas ||
			st.ReadyReplicas != d.Status.ReadyReplicas || st.AvailableReplicas != d.Status.AvailableReplicas ||
			d.Generation != d.Status.ObservedGeneration
		var prog appsv1.DeploymentCondition
		switch {
		case complete:
			prog = mk(appsv1.DeploymentProgressing, true, "NewReplicaSetAvailable", fmt.Sprintf("ReplicaSet %q has successfully progressed.", rsName), false)
		default:
			prog = mk(appsv1.DeploymentProgressing, true, "ReplicaSetUpdated", fmt.Sprintf("ReplicaSet %q is progressing.", rsName), progressed)
			deadline := time.Duration(ptr.Deref(d.Spec.ProgressDeadlineSeconds, 600)) * time.Second
			if old := find(appsv1.DeploymentProgressing); old != nil && !progressed {
				if old.Reason == "ProgressDeadlineExceeded" || (old.Reason == "ReplicaSetUpdated" && now.Sub(old.LastUpdateTime.Time) > deadline) {
					prog = mk(appsv1.DeploymentProgressing, false, "ProgressDeadlineExceeded", fmt.Sprintf("ReplicaSet %q has timed out progressing.", rsName), false)
				}
			}
		}
		st.Conditions = []appsv1.DeploymentCondition{avail, prog}
		if d.Spec.Paused {
			st.Conditions[1] = mk(appsv1.DeploymentProgressing, false, "DeploymentPaused", "Deployment is paused", false)
		}
		d.Status = st
		_, _ = c.Store.Update(gvrDeployments, d)
	}
}

// ---------------------------------------------------------------------------
// ReplicaSets

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (c *Cluster) ownedPods(uid string, ns string) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range listTyped[*corev1.Pod](c, gvrPods, ns) {
		if isControlledBy(p, types.UID(uid)) {
			out = append(out, p)
		}
	}
	return out
}

func (c *Cluster) syncReplicaSets() {
	for _, rs := range listTyped[*appsv1.ReplicaSet](c, gvrReplicaSets, "") {
		pods := c.ownedPods(string(rs.UID), rs.Namespace)
		var active []*corev1.Pod
		for _, p := range pods {
			if activePod(p) {
				active = append(active, p)
			}
		}
		diff := len(active) - int(*rs.Spec.Replicas)
		switch {
		case diff < 0:
			for i := 0; i < -diff; i++ {
				tmpl := rs.Spec.Template.DeepCopy()
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: rs.Name + "-" + c.suffix(5), Namespace: rs.Namespace,
						Labels: tmpl.Labels, Annotations: tmpl.Annotations,
						OwnerReferences: []metav1.OwnerReference{controllerRef("ReplicaSet", rs.Name, string(rs.UID))},
					},
					Spec: tmpl.Spec,
				}
				if _, err := c.create(res("pods"), pod, false); err != nil {
					c.recordEvent(rs, "ReplicaSet", corev1.EventTypeWarning, "FailedCreate", "Error creating: "+err.Error(), "replicaset-controller", "")
					break
				}
				c.recordEvent(rs, "ReplicaSet", corev1.EventTypeNormal, "SuccessfulCreate", "Created pod: "+pod.Name, "replicaset-controller", "")
			}
		case diff > 0:
			// Delete the least valuable pods first: unscheduled, pending, not ready, newest.
			sort.SliceStable(active, func(i, j int) bool {
				a, b := active[i], active[j]
				if (a.Spec.NodeName == "") != (b.Spec.NodeName == "") {
					return a.Spec.NodeName == ""
				}
				if (a.Status.Phase == corev1.PodPending) != (b.Status.Phase == corev1.PodPending) {
					return a.Status.Phase == corev1.PodPending
				}
				if podReady(a) != podReady(b) {
					return !podReady(a)
				}
				return b.CreationTimestamp.Before(&a.CreationTimestamp) || (a.CreationTimestamp.Equal(&b.CreationTimestamp) && a.Name > b.Name)
			})
			for _, p := range active[:diff] {
				if err := c.delete(res("pods"), p, metav1.DeletePropagationBackground); err == nil {
					c.recordEvent(rs, "ReplicaSet", corev1.EventTypeNormal, "SuccessfulDelete", "Deleted pod: "+p.Name, "replicaset-controller", "")
				}
			}
		}
	}
}

func (c *Cluster) syncReplicaSetStatus() {
	for _, rs := range listTyped[*appsv1.ReplicaSet](c, gvrReplicaSets, "") {
		var st appsv1.ReplicaSetStatus
		st.ObservedGeneration = rs.Generation
		sel, _ := metav1.LabelSelectorAsSelector(rs.Spec.Selector)
		for _, p := range c.ownedPods(string(rs.UID), rs.Namespace) {
			if !activePod(p) {
				continue
			}
			st.Replicas++
			if sel != nil && sel.Matches(labels.Set(rs.Spec.Template.Labels)) {
				st.FullyLabeledReplicas++
			}
			if podReady(p) {
				st.ReadyReplicas++
				st.AvailableReplicas++
			}
		}
		st.Conditions = rs.Status.Conditions
		rs.Status = st
		_, _ = c.Store.Update(gvrReplicaSets, rs)
	}
}

// ---------------------------------------------------------------------------
// Endpoints / EndpointSlices

func (c *Cluster) syncEndpoints() {
	for _, svc := range listTyped[*corev1.Service](c, gvrServices, "") {
		if svc.Name == "kubernetes" && svc.Namespace == "default" {
			c.ensureKubernetesEndpoints(svc)
			continue
		}
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		var ready, notReady []corev1.EndpointAddress
		var sliceEps []discoveryv1.Endpoint
		var ports []corev1.EndpointPort
		portsSet := false
		for _, p := range listTyped[*corev1.Pod](c, gvrPods, svc.Namespace) {
			if !sel.Matches(labels.Set(p.Labels)) || p.Status.PodIP == "" || !activePod(p) {
				continue
			}
			addr := corev1.EndpointAddress{IP: p.Status.PodIP, NodeName: ptr.To(p.Spec.NodeName),
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID}}
			r := podReady(p)
			if r {
				ready = append(ready, addr)
			} else {
				notReady = append(notReady, addr)
			}
			sliceEps = append(sliceEps, discoveryv1.Endpoint{Addresses: []string{p.Status.PodIP},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(r), Serving: ptr.To(r), Terminating: ptr.To(false)},
				NodeName:   ptr.To(p.Spec.NodeName), TargetRef: addr.TargetRef})
			if !portsSet {
				portsSet = true
				for _, sp := range svc.Spec.Ports {
					ports = append(ports, corev1.EndpointPort{Name: sp.Name, Port: resolveTargetPort(sp, p), Protocol: sp.Protocol})
				}
			}
		}
		ep := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace, Labels: svc.Labels}}
		if len(ready)+len(notReady) > 0 {
			ep.Subsets = []corev1.EndpointSubset{{Addresses: ready, NotReadyAddresses: notReady, Ports: ports}}
		}
		c.upsertEndpoints(ep)
		slicePorts := make([]discoveryv1.EndpointPort, 0, len(ports))
		for _, p := range ports {
			slicePorts = append(slicePorts, discoveryv1.EndpointPort{Name: ptr.To(p.Name), Port: ptr.To(p.Port), Protocol: ptr.To(p.Protocol)})
		}
		if len(ports) == 0 {
			for _, sp := range svc.Spec.Ports {
				slicePorts = append(slicePorts, discoveryv1.EndpointPort{Name: ptr.To(sp.Name), Port: ptr.To(sp.TargetPort.IntVal), Protocol: ptr.To(sp.Protocol)})
			}
		}
		es := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: svc.Name + "-" + randString(hash64(svc.Namespace, svc.Name), 5), Namespace: svc.Namespace,
				Labels: map[string]string{discoveryv1.LabelServiceName: svc.Name, discoveryv1.LabelManagedBy: "endpointslice-controller.k8s.io"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: svc.Name, UID: svc.UID,
					Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}},
			AddressType: discoveryv1.AddressTypeIPv4, Endpoints: sliceEps, Ports: slicePorts,
		}
		c.upsertSlice(es)
	}
	// Remove endpoints of deleted services.
	for _, ep := range listTyped[*corev1.Endpoints](c, gvrEndpoints, "") {
		if _, err := c.Store.Get(gvrServices, ep.Namespace, ep.Name); err != nil {
			_, _ = c.Store.Delete(gvrEndpoints, ep.Namespace, ep.Name)
		}
	}
}

func resolveTargetPort(sp corev1.ServicePort, p *corev1.Pod) int32 {
	if sp.TargetPort.Type == intstr.Int {
		if sp.TargetPort.IntVal == 0 {
			return sp.Port
		}
		return sp.TargetPort.IntVal
	}
	for _, ct := range p.Spec.Containers {
		for _, cp := range ct.Ports {
			if cp.Name == sp.TargetPort.StrVal {
				return cp.ContainerPort
			}
		}
	}
	return sp.Port
}

func (c *Cluster) ensureKubernetesEndpoints(svc *corev1.Service) {
	ep := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"endpointslice.kubernetes.io/skip-mirror": "true"}},
		Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: DefaultNodes()[0].IP}},
			Ports: []corev1.EndpointPort{{Name: "https", Port: 6443, Protocol: corev1.ProtocolTCP}}}}}
	c.upsertEndpoints(ep)
	es := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{DefaultNodes()[0].IP}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To[int32](6443), Protocol: ptr.To(corev1.ProtocolTCP)}},
	}
	c.upsertSlice(es)
}

func (c *Cluster) upsertEndpoints(ep *corev1.Endpoints) {
	cur, err := c.Store.Get(gvrEndpoints, ep.Namespace, ep.Name)
	if err != nil {
		_, _ = c.create(res("endpoints"), ep, false)
		return
	}
	e := cur.(*corev1.Endpoints)
	e.Subsets = ep.Subsets
	e.Labels = ep.Labels
	_, _ = c.Store.Update(gvrEndpoints, e)
}

func (c *Cluster) upsertSlice(es *discoveryv1.EndpointSlice) {
	cur, err := c.Store.Get(gvrEndpointSlices, es.Namespace, es.Name)
	if err != nil {
		_, _ = c.create(res("endpointslices"), es, false)
		return
	}
	e := cur.(*discoveryv1.EndpointSlice)
	e.Endpoints = es.Endpoints
	e.Ports = es.Ports
	e.Labels = es.Labels
	_, _ = c.Store.Update(gvrEndpointSlices, e)
}
