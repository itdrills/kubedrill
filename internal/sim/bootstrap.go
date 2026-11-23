package sim

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/itdrills/kubedrill/internal/brand"
	"github.com/itdrills/kubedrill/internal/registry"
)

// KubernetesVersion is the version reported by the simulated cluster.
const KubernetesVersion = "v1.37.2"

var (
	gvrNamespaces  = corev1.SchemeGroupVersion.WithResource("namespaces")
	gvrNodes       = corev1.SchemeGroupVersion.WithResource("nodes")
	gvrPods        = corev1.SchemeGroupVersion.WithResource("pods")
	gvrServices    = corev1.SchemeGroupVersion.WithResource("services")
	gvrEndpoints   = corev1.SchemeGroupVersion.WithResource("endpoints")
	gvrConfigMaps  = corev1.SchemeGroupVersion.WithResource("configmaps")
	gvrSecrets     = corev1.SchemeGroupVersion.WithResource("secrets")
	gvrSAs         = corev1.SchemeGroupVersion.WithResource("serviceaccounts")
	gvrDeployments = appsv1.SchemeGroupVersion.WithResource("deployments")
	gvrReplicaSets = appsv1.SchemeGroupVersion.WithResource("replicasets")
)

func res(gvrResource string) *registry.Resource {
	for _, r := range registry.All() {
		if r.GVR.Resource == gvrResource {
			return r
		}
	}
	panic("unknown resource " + gvrResource)
}

func labelsSet(m map[string]string) labels.Set { return labels.Set(m) }

// NodeSpec describes a simulated node.
type NodeSpec struct {
	Name         string
	ControlPlane bool
	CPU          string
	Memory       string
	IP           string
}

// DefaultNodes is the simulated topology: one control plane, two workers.
func DefaultNodes() []NodeSpec {
	return []NodeSpec{
		{Name: brand.NodePrefix + "-control-plane", ControlPlane: true, CPU: "4", Memory: "8Gi", IP: "172.18.0.2"},
		{Name: brand.NodePrefix + "-worker", CPU: "2", Memory: "4Gi", IP: "172.18.0.3"},
		{Name: brand.NodePrefix + "-worker2", CPU: "2", Memory: "4Gi", IP: "172.18.0.4"},
	}
}

func (c *Cluster) mustCreate(r *registry.Resource, obj runtime.Object) {
	if _, err := c.create(r, obj, false); err != nil {
		panic(fmt.Sprintf("bootstrap %s: %v", r.Kind, err))
	}
}

func (c *Cluster) bootstrap() {
	for _, ns := range []string{"default", "kube-system", "kube-public", "kube-node-lease"} {
		c.mustCreate(res("namespaces"), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	for i, n := range DefaultNodes() {
		c.mustCreate(res("nodes"), c.newNode(n, i))
	}
	c.mustCreate(res("services"), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default", Labels: map[string]string{"component": "apiserver", "provider": "kubernetes"}},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.1", Ports: []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt32(6443)}}},
	})
	c.mustCreate(res("services"), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns", "kubernetes.io/name": "CoreDNS"}},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10", Selector: map[string]string{"k8s-app": "kube-dns"}, Ports: []corev1.ServicePort{
			{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)},
			{Name: "dns-tcp", Port: 53, TargetPort: intstr.FromInt32(53)},
			{Name: "metrics", Port: 9153, TargetPort: intstr.FromInt32(9153)},
		}},
	})
	c.mustCreate(res("configmaps"), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"},
		Data:       map[string]string{"Corefile": ".:53 {\n    errors\n    health\n    ready\n    kubernetes cluster.local in-addr.arpa ip6.arpa\n    forward . /etc/resolv.conf\n    cache 30\n}\n"},
	})
	c.mustCreate(res("deployments"), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": "kube-dns"}},
				Spec: corev1.PodSpec{
					PriorityClassName: "system-cluster-critical",
					Tolerations: []corev1.Toleration{
						{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
						{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule},
					},
					NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
					Containers: []corev1.Container{{
						Name: "coredns", Image: "registry.k8s.io/coredns/coredns:v1.12.1",
						Args: []string{"-conf", "/etc/coredns/Corefile"},
						Ports: []corev1.ContainerPort{
							{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
							{Name: "dns-tcp", ContainerPort: 53},
							{Name: "metrics", ContainerPort: 9153},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("70Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("170Mi")},
						},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8181)}}},
						LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)}}, InitialDelaySeconds: 60},
						VolumeMounts:   []corev1.VolumeMount{{Name: "config-volume", MountPath: "/etc/coredns", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{Name: "config-volume", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "coredns"},
						Items:                []corev1.KeyToPath{{Key: "Corefile", Path: "Corefile"}},
					}}}},
				},
			},
		},
	})
	cp := DefaultNodes()[0].Name
	for _, comp := range []struct{ name, image, cpu string }{
		{"etcd", "registry.k8s.io/etcd:3.6.4-0", "100m"},
		{"kube-apiserver", "registry.k8s.io/kube-apiserver:" + KubernetesVersion, "250m"},
		{"kube-controller-manager", "registry.k8s.io/kube-controller-manager:" + KubernetesVersion, "200m"},
		{"kube-scheduler", "registry.k8s.io/kube-scheduler:" + KubernetesVersion, "100m"},
	} {
		c.mustCreate(res("pods"), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: comp.name + "-" + cp, Namespace: "kube-system",
				Labels:      map[string]string{"component": comp.name, "tier": "control-plane"},
				Annotations: map[string]string{"kubernetes.io/config.source": "file", "kubedrill.itdrills.github.io/static": "true"}},
			Spec: corev1.PodSpec{
				NodeName: cp, HostNetwork: true, PriorityClassName: "system-node-critical",
				Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}},
				Containers: []corev1.Container{{Name: comp.name, Image: comp.image,
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(comp.cpu)}}}},
			},
		})
	}
	// Pre-pulled control-plane images: the static pods start immediately.
	c.st.NodeImages[cp] = append(c.st.NodeImages[cp],
		"registry.k8s.io/etcd:3.6.4-0", "registry.k8s.io/kube-apiserver:"+KubernetesVersion,
		"registry.k8s.io/kube-controller-manager:"+KubernetesVersion, "registry.k8s.io/kube-scheduler:"+KubernetesVersion)
	c.ensureIngressAddon()
}

func (c *Cluster) newNode(n NodeSpec, idx int) *corev1.Node {
	lbls := map[string]string{
		"beta.kubernetes.io/arch": "arm64", "beta.kubernetes.io/os": "linux",
		"kubernetes.io/arch": "arm64", "kubernetes.io/hostname": n.Name, "kubernetes.io/os": "linux",
	}
	var taints []corev1.Taint
	if n.ControlPlane {
		lbls["node-role.kubernetes.io/control-plane"] = ""
		lbls["ingress-ready"] = "true"
		lbls["node.kubernetes.io/exclude-from-external-load-balancers"] = ""
		taints = append(taints, corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule})
	}
	cpu := resource.MustParse(n.CPU)
	mem := resource.MustParse(n.Memory)
	allocMem := mem.DeepCopy()
	allocMem.Sub(resource.MustParse("100Mi"))
	now := c.now()
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: n.Name, Labels: lbls, Annotations: map[string]string{
			"node.alpha.kubernetes.io/ttl": "0", "volumes.kubernetes.io/controller-managed-attach-detach": "true",
			"kubedrill.itdrills.github.io/simulated": "true"}},
		Spec: corev1.NodeSpec{PodCIDR: fmt.Sprintf("10.244.%d.0/24", idx), PodCIDRs: []string{fmt.Sprintf("10.244.%d.0/24", idx)},
			ProviderID: "kubedrill://" + n.Name, Taints: taints},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem,
				corev1.ResourcePods: resource.MustParse("110"), corev1.ResourceEphemeralStorage: resource.MustParse("50Gi")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: allocMem,
				corev1.ResourcePods: resource.MustParse("110"), corev1.ResourceEphemeralStorage: resource.MustParse("50Gi")},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientMemory", Message: "kubelet has sufficient memory available", LastHeartbeatTime: now, LastTransitionTime: now},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasNoDiskPressure", Message: "kubelet has no disk pressure", LastHeartbeatTime: now, LastTransitionTime: now},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientPID", Message: "kubelet has sufficient PID available", LastHeartbeatTime: now, LastTransitionTime: now},
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady", Message: "kubelet is posting ready status (simulated)", LastHeartbeatTime: now, LastTransitionTime: now},
			},
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: n.IP}, {Type: corev1.NodeHostName, Address: n.Name}},
			NodeInfo: corev1.NodeSystemInfo{
				MachineID: fmt.Sprintf("%032x", hash64("machine", n.Name)), SystemUUID: string(c.newUID()), BootID: string(c.newUID()),
				KernelVersion: "6.10.14-simulated", OSImage: "KubeDrill Simulated Node", ContainerRuntimeVersion: "simulated://1.0",
				KubeletVersion: KubernetesVersion, KubeProxyVersion: KubernetesVersion, OperatingSystem: "linux", Architecture: "arm64",
			},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250}},
		},
	}
}
