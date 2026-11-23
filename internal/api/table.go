package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/itskills/kubedrill/internal/registry"
)

// wantsTable reports whether kubectl asked for server-side printing.
func wantsTable(r *http.Request) bool {
	for _, a := range r.Header.Values("Accept") {
		if strings.Contains(a, "as=Table") {
			return true
		}
	}
	return false
}

type col struct {
	name, typ, format, desc string
	priority                int32
}

func columns(cols ...col) []metav1.TableColumnDefinition {
	out := make([]metav1.TableColumnDefinition, 0, len(cols))
	for _, c := range cols {
		t := c.typ
		if t == "" {
			t = "string"
		}
		out = append(out, metav1.TableColumnDefinition{Name: c.name, Type: t, Format: c.format, Description: c.desc, Priority: c.priority})
	}
	return out
}

var nameCol = col{name: "Name", format: "name", desc: "Name must be unique within a namespace."}
var ageCol = col{name: "Age", desc: "CreationTimestamp is a timestamp representing the server time when this object was created."}

func (s *Server) age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(s.Cluster.Clock.Now().Sub(t.Time))
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func selectorString(sel *metav1.LabelSelector) string {
	if sel == nil {
		return "<none>"
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "<error>"
	}
	return orNone(s.String())
}

func mapSelector(m map[string]string) string {
	if len(m) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func containersImages(spec corev1.PodSpec) (string, string) {
	var names, images []string
	for _, c := range spec.Containers {
		names = append(names, c.Name)
		images = append(images, c.Image)
	}
	return strings.Join(names, ","), strings.Join(images, ",")
}

// toTable renders objects as a meta.k8s.io/v1 Table.
func (s *Server) toTable(res *registry.Resource, objs []runtime.Object, r *http.Request, withColumns bool, rv string) *metav1.Table {
	t := &metav1.Table{TypeMeta: metav1.TypeMeta{Kind: "Table", APIVersion: "meta.k8s.io/v1"}}
	t.ResourceVersion = rv
	var defs []metav1.TableColumnDefinition
	include := r.URL.Query().Get("includeObject")
	for _, o := range objs {
		cells := s.cells(res, o)
		row := metav1.TableRow{Cells: cells}
		switch include {
		case "None":
		case "Object":
			b, _ := json.Marshal(encodeObject(res, o))
			row.Object = runtime.RawExtension{Raw: b}
		default:
			m, _ := meta.Accessor(o)
			pom := metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1"}}
			pom.ObjectMeta = metav1.ObjectMeta{Name: m.GetName(), Namespace: m.GetNamespace(), UID: m.GetUID(), ResourceVersion: m.GetResourceVersion(),
				CreationTimestamp: m.GetCreationTimestamp(), Labels: m.GetLabels(), Annotations: m.GetAnnotations(), OwnerReferences: m.GetOwnerReferences(),
				Generation: m.GetGeneration(), DeletionTimestamp: m.GetDeletionTimestamp()}
			b, _ := json.Marshal(pom)
			row.Object = runtime.RawExtension{Raw: b}
		}
		t.Rows = append(t.Rows, row)
	}
	if withColumns {
		defs = s.columnDefs(res)
		t.ColumnDefinitions = defs
	}
	if t.Rows == nil {
		t.Rows = []metav1.TableRow{}
	}
	return t
}

func (s *Server) columnDefs(res *registry.Resource) []metav1.TableColumnDefinition {
	switch res.Kind {
	case "Pod":
		return columns(nameCol,
			col{name: "Ready", desc: "The aggregate readiness state of this pod for accepting traffic."},
			col{name: "Status", desc: "The aggregate status of the containers in this pod."},
			col{name: "Restarts", desc: "The number of times the containers in this pod have been restarted and when the last container in this pod has restarted."},
			ageCol,
			col{name: "IP", desc: "IP address allocated to the pod.", priority: 1},
			col{name: "Node", desc: "Name of the node the pod is assigned to.", priority: 1},
			col{name: "Nominated Node", desc: "Nominated node name.", priority: 1},
			col{name: "Readiness Gates", desc: "Readiness gates.", priority: 1})
	case "Deployment":
		return columns(nameCol,
			col{name: "Ready", desc: "Number of the pod with ready state"},
			col{name: "Up-to-date", typ: "integer", desc: "Total number of non-terminated pods targeted by this deployment that have the desired template spec."},
			col{name: "Available", typ: "integer", desc: "Total number of available pods (ready for at least minReadySeconds) targeted by this deployment."},
			ageCol,
			col{name: "Containers", desc: "Names of each container in the template.", priority: 1},
			col{name: "Images", desc: "Images referenced by each container in the template.", priority: 1},
			col{name: "Selector", desc: "Label selector for pods.", priority: 1})
	case "ReplicaSet":
		return columns(nameCol,
			col{name: "Desired", typ: "integer", desc: "Replicas is the number of desired replicas."},
			col{name: "Current", typ: "integer", desc: "Replicas is the most recently observed number of replicas."},
			col{name: "Ready", typ: "integer", desc: "The number of ready replicas for this replica set."},
			ageCol,
			col{name: "Containers", desc: "Names of each container in the template.", priority: 1},
			col{name: "Images", desc: "Images referenced by each container in the template.", priority: 1},
			col{name: "Selector", desc: "Label selector for pods.", priority: 1})
	case "Service":
		return columns(nameCol,
			col{name: "Type", desc: "Type of the service."},
			col{name: "Cluster-IP", desc: "IP address of the service (simulated, not routable)."},
			col{name: "External-IP", desc: "External IP addresses (none in the simulator)."},
			col{name: "Port(s)", desc: "The ports that are exposed by this service."},
			ageCol,
			col{name: "Selector", desc: "Route service traffic to pods with label keys and values matching this selector.", priority: 1})
	case "Node":
		return columns(nameCol,
			col{name: "Status", desc: "The status of the node"},
			col{name: "Roles", desc: "The roles of the node"},
			ageCol,
			col{name: "Version", desc: "Kubelet Version reported by the node."},
			col{name: "Internal-IP", desc: "List of addresses reachable to the node.", priority: 1},
			col{name: "External-IP", desc: "List of addresses reachable to the node.", priority: 1},
			col{name: "OS-Image", desc: "OS Image reported by the node.", priority: 1},
			col{name: "Kernel-Version", desc: "Kernel Version reported by the node.", priority: 1},
			col{name: "Container-Runtime", desc: "Container Runtime Version reported by the node.", priority: 1})
	case "Namespace":
		return columns(nameCol, col{name: "Status", desc: "The status of the namespace"}, ageCol)
	case "ConfigMap":
		return columns(nameCol, col{name: "Data", typ: "integer", desc: "Number of keys."}, ageCol)
	case "Secret":
		return columns(nameCol, col{name: "Type", desc: "The type of the secret"}, col{name: "Data", typ: "integer", desc: "Number of keys."}, ageCol)
	case "ServiceAccount":
		return columns(nameCol, col{name: "Secrets", typ: "integer", desc: "Number of secrets."}, ageCol)
	case "Endpoints":
		return columns(nameCol, col{name: "Endpoints", desc: "Endpoints of the service."}, ageCol)
	case "EndpointSlice":
		return columns(nameCol, col{name: "AddressType", desc: "Address type."}, col{name: "Ports", desc: "Ports."}, col{name: "Endpoints", desc: "Endpoints."}, ageCol)
	case "Ingress":
		return columns(nameCol,
			col{name: "Class", desc: "The name of the IngressClass resource that should be used for this Ingress."},
			col{name: "Hosts", desc: "Hosts that incoming requests are matched against."},
			col{name: "Address", desc: "Address is a list containing ingress points for the load-balancer."},
			col{name: "Ports", desc: "Ports of TLS configurations that open."},
			ageCol)
	case "IngressClass":
		return columns(nameCol,
			col{name: "Controller", desc: "Controller that is responsible for handling this class."},
			col{name: "Parameters", desc: "A reference to a resource with additional parameters."},
			ageCol)
	case "Event":
		return columns(
			col{name: "Last Seen", desc: "Time when last event occurred."},
			col{name: "Type", desc: "Type of this event (Normal, Warning)."},
			col{name: "Reason", desc: "Reason is why the action was taken."},
			col{name: "Object", desc: "The object that this event is about."},
			col{name: "Subobject", desc: "The subobject.", priority: 1},
			col{name: "Source", desc: "The component reporting this event.", priority: 1},
			col{name: "Message", desc: "A human-readable description of the status of this operation."},
			col{name: "First Seen", desc: "The time at which the event was first recorded.", priority: 1},
			col{name: "Count", desc: "The number of times this event has occurred.", priority: 1},
			col{name: "Name", desc: "Name of the event.", priority: 1})
	}
	return columns(nameCol, ageCol)
}

func (s *Server) cells(res *registry.Resource, o runtime.Object) []any {
	switch x := o.(type) {
	case *corev1.Pod:
		return s.podCells(x)
	case *appsv1.Deployment:
		c, i := containersImages(x.Spec.Template.Spec)
		return []any{x.Name, fmt.Sprintf("%d/%d", x.Status.ReadyReplicas, *x.Spec.Replicas), int64(x.Status.UpdatedReplicas), int64(x.Status.AvailableReplicas), s.age(x.CreationTimestamp), c, i, selectorString(x.Spec.Selector)}
	case *appsv1.ReplicaSet:
		c, i := containersImages(x.Spec.Template.Spec)
		return []any{x.Name, int64(*x.Spec.Replicas), int64(x.Status.Replicas), int64(x.Status.ReadyReplicas), s.age(x.CreationTimestamp), c, i, selectorString(x.Spec.Selector)}
	case *corev1.Service:
		return []any{x.Name, string(x.Spec.Type), orNone(x.Spec.ClusterIP), externalIP(x), servicePorts(x), s.age(x.CreationTimestamp), mapSelector(x.Spec.Selector)}
	case *corev1.Node:
		return s.nodeCells(x)
	case *corev1.Namespace:
		return []any{x.Name, string(x.Status.Phase), s.age(x.CreationTimestamp)}
	case *corev1.ConfigMap:
		return []any{x.Name, int64(len(x.Data) + len(x.BinaryData)), s.age(x.CreationTimestamp)}
	case *corev1.Secret:
		return []any{x.Name, string(x.Type), int64(len(x.Data)), s.age(x.CreationTimestamp)}
	case *corev1.ServiceAccount:
		return []any{x.Name, int64(len(x.Secrets)), s.age(x.CreationTimestamp)}
	case *corev1.Endpoints:
		return []any{x.Name, endpointsString(x), s.age(x.CreationTimestamp)}
	case *discoveryv1.EndpointSlice:
		var ports, eps []string
		for _, p := range x.Ports {
			if p.Port != nil {
				ports = append(ports, fmt.Sprint(*p.Port))
			}
		}
		for _, e := range x.Endpoints {
			eps = append(eps, e.Addresses...)
		}
		return []any{x.Name, string(x.AddressType), orNone(strings.Join(ports, ",")), orNone(strings.Join(eps, ",")), s.age(x.CreationTimestamp)}
	case *networkingv1.Ingress:
		return []any{x.Name, ingressClassOf(x), ingressHosts(x), ingressAddress(x), ingressPorts(x), s.age(x.CreationTimestamp)}
	case *networkingv1.IngressClass:
		params := "<none>"
		if p := x.Spec.Parameters; p != nil {
			params = p.Kind + "/" + p.Name
			if p.APIGroup != nil && *p.APIGroup != "" {
				params = *p.APIGroup + "/" + params
			}
		}
		return []any{x.Name, x.Spec.Controller, params, s.age(x.CreationTimestamp)}
	case *corev1.Event:
		obj := strings.ToLower(x.InvolvedObject.Kind) + "/" + x.InvolvedObject.Name
		src := x.Source.Component
		if x.Source.Host != "" {
			src += ", " + x.Source.Host
		}
		return []any{s.age(x.LastTimestamp), x.Type, x.Reason, obj, x.InvolvedObject.FieldPath, src, strings.TrimSpace(x.Message), s.age(x.FirstTimestamp), int64(x.Count), x.Name}
	}
	m, _ := meta.Accessor(o)
	return []any{m.GetName(), s.age(m.GetCreationTimestamp())}
}

func externalIP(svc *corev1.Service) string {
	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		return "<pending>"
	case corev1.ServiceTypeExternalName:
		return svc.Spec.ExternalName
	}
	if len(svc.Spec.ExternalIPs) > 0 {
		return strings.Join(svc.Spec.ExternalIPs, ",")
	}
	return "<none>"
}

func servicePorts(svc *corev1.Service) string {
	var parts []string
	for _, p := range svc.Spec.Ports {
		s := fmt.Sprintf("%d", p.Port)
		if p.NodePort != 0 {
			s += fmt.Sprintf(":%d", p.NodePort)
		}
		parts = append(parts, s+"/"+string(p.Protocol))
	}
	return orNone(strings.Join(parts, ","))
}

func endpointsString(ep *corev1.Endpoints) string {
	var list []string
	for _, ss := range ep.Subsets {
		for _, a := range ss.Addresses {
			for _, p := range ss.Ports {
				list = append(list, fmt.Sprintf("%s:%d", a.IP, p.Port))
			}
			if len(ss.Ports) == 0 {
				list = append(list, a.IP)
			}
		}
	}
	if len(list) == 0 {
		return "<none>"
	}
	if len(list) > 3 {
		return strings.Join(list[:3], ",") + fmt.Sprintf(" + %d more...", len(list)-3)
	}
	return strings.Join(list, ",")
}

func (s *Server) nodeCells(n *corev1.Node) []any {
	status := "Unknown"
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				status = "Ready"
			} else {
				status = "NotReady"
			}
		}
	}
	if n.Spec.Unschedulable {
		status += ",SchedulingDisabled"
	}
	var roles []string
	for k := range n.Labels {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			roles = append(roles, strings.TrimPrefix(k, "node-role.kubernetes.io/"))
		}
	}
	sort.Strings(roles)
	internal, external := "<none>", "<none>"
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			internal = a.Address
		}
		if a.Type == corev1.NodeExternalIP {
			external = a.Address
		}
	}
	return []any{n.Name, status, orNone(strings.Join(roles, ",")), s.age(n.CreationTimestamp), n.Status.NodeInfo.KubeletVersion,
		internal, external, n.Status.NodeInfo.OSImage, n.Status.NodeInfo.KernelVersion, n.Status.NodeInfo.ContainerRuntimeVersion}
}

// podCells mirrors the status logic of kubectl's pod printer.
func (s *Server) podCells(p *corev1.Pod) []any {
	restarts := 0
	total := len(p.Spec.Containers)
	ready := 0
	var lastRestart time.Time
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	hasRunning := false
	for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
		cs := p.Status.ContainerStatuses[i]
		restarts += int(cs.RestartCount)
		if cs.LastTerminationState.Terminated != nil {
			if t := cs.LastTerminationState.Terminated.FinishedAt.Time; t.After(lastRestart) {
				lastRestart = t
			}
		}
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
			reason = cs.State.Terminated.Reason
		case cs.State.Terminated != nil:
			if cs.State.Terminated.Signal != 0 {
				reason = fmt.Sprintf("Signal:%d", cs.State.Terminated.Signal)
			} else {
				reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
			}
		case cs.Ready && cs.State.Running != nil:
			hasRunning = true
			ready++
		}
	}
	if reason == "Completed" && hasRunning {
		reason = "Running"
	}
	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	restartStr := fmt.Sprint(restarts)
	if restarts > 0 && !lastRestart.IsZero() {
		restartStr = fmt.Sprintf("%d (%s ago)", restarts, duration.HumanDuration(s.Cluster.Clock.Now().Sub(lastRestart)))
	}
	node := orNone(p.Spec.NodeName)
	ip := orNone(p.Status.PodIP)
	gates := "<none>"
	return []any{p.Name, fmt.Sprintf("%d/%d", ready, total), reason, restartStr, s.age(p.CreationTimestamp), ip, node, "<none>", gates}
}

func ingressClassOf(ing *networkingv1.Ingress) string {
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName
	}
	return "<none>"
}

// ingressHosts mirrors kubectl's formatHosts: up to three hosts, "*" for
// rules without host.
func ingressHosts(ing *networkingv1.Ingress) string {
	var hosts []string
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	if len(hosts) == 0 {
		return "*"
	}
	if len(hosts) > 3 {
		return fmt.Sprintf("%s + %d more...", strings.Join(hosts[:3], ","), len(hosts)-3)
	}
	return strings.Join(hosts, ",")
}

func ingressAddress(ing *networkingv1.Ingress) string {
	var out []string
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.IP != "" {
			out = append(out, lb.IP)
		} else if lb.Hostname != "" {
			out = append(out, lb.Hostname)
		}
	}
	return strings.Join(out, ",")
}

func ingressPorts(ing *networkingv1.Ingress) string {
	if len(ing.Spec.TLS) > 0 {
		return "80, 443"
	}
	return "80"
}
