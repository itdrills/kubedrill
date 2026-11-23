package sim

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Container phases of the simulated runtime.
const (
	cNew         = ""
	cPulling     = "Pulling"
	cPullFailed  = "PullFailed"
	cPullBackOff = "PullBackOff"
	cCreating    = "Creating"
	cRunning     = "Running"
	cTerminated  = "Terminated"
	cDone        = "Done"
)

type termInfo struct {
	ExitCode    int32     `json:"exitCode"`
	Reason      string    `json:"reason"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	ContainerID string    `json:"containerID"`
}

type ctrRT struct {
	Name          string    `json:"name"`
	Image         string    `json:"image"`
	Phase         string    `json:"phase"`
	NextAt        time.Time `json:"nextAt"`
	PullBackoff   int       `json:"pullBackoff"`
	PullError     string    `json:"pullError,omitempty"`
	PullMessage   string    `json:"pullMessage,omitempty"`
	ImageDigest   string    `json:"imageDigest,omitempty"`
	ConfigError   string    `json:"configError,omitempty"`
	Restarts      int32     `json:"restarts"`
	CrashBackoff  int       `json:"crashBackoff"`
	StartedAt     time.Time `json:"startedAt"`
	ContainerID   string    `json:"containerID"`
	ImageID       string    `json:"imageID"`
	Plan          RunPlan   `json:"plan"`
	Term          *termInfo `json:"term,omitempty"`
	ShowTermUntil time.Time `json:"showTermUntil"`
	Ready         bool      `json:"ready"`
	ProbeFails    int32     `json:"probeFails"`
	ProbeOK       int32     `json:"probeOK"`
	NextProbe     time.Time `json:"nextProbe"`
	LiveFails     int32     `json:"liveFails"`
	NextLive      time.Time `json:"nextLive"`
	Logs          []string  `json:"logs"`
	PrevLogs      []string  `json:"prevLogs"`
}

type podRT struct {
	Node       string    `json:"node"`
	IP         string    `json:"ip"`
	StartTime  time.Time `json:"startTime"`
	NextMount  time.Time `json:"nextMount"`
	Containers []*ctrRT  `json:"containers"`
}

func (p *podRT) ctr(name string) *ctrRT {
	for _, c := range p.Containers {
		if c.Name == name {
			return c
		}
	}
	c := &ctrRT{Name: name}
	p.Containers = append(p.Containers, c)
	return c
}

func (c *Cluster) nodeIndex(name string) int {
	for i, n := range DefaultNodes() {
		if n.Name == name {
			return i
		}
	}
	return int(hash64(name)%200) + 10
}

func (c *Cluster) hex(n int) string {
	var b strings.Builder
	for b.Len() < n {
		fmt.Fprintf(&b, "%016x", hash64(c.seed, "hex", fmt.Sprint(c.next())))
	}
	return b.String()[:n]
}

// syncKubelet simulates the kubelet of every node for every bound pod.
func (c *Cluster) syncKubelet() {
	now := c.Clock.Now()
	pods := listTyped[*corev1.Pod](c, gvrPods, "")
	alive := map[string]bool{}
	for _, pod := range pods {
		alive[string(pod.UID)] = true
		if pod.Spec.NodeName == "" {
			continue
		}
		rt := c.st.Pods[string(pod.UID)]
		if rt == nil {
			rt = &podRT{Node: pod.Spec.NodeName, StartTime: now}
			c.st.Pods[string(pod.UID)] = rt
		}
		c.syncPod(pod, rt, now)
	}
	for uid := range c.st.Pods {
		if !alive[uid] {
			delete(c.st.Pods, uid)
		}
	}
}

func (c *Cluster) podDeleted(pod *corev1.Pod) {
	if rt := c.st.Pods[string(pod.UID)]; rt != nil {
		for _, ct := range rt.Containers {
			if ct.Phase == cRunning {
				c.recordEvent(pod, "Pod", corev1.EventTypeNormal, "Killing", "Stopping container "+ct.Name, "kubelet", pod.Spec.NodeName)
			}
		}
	}
	delete(c.st.Pods, string(pod.UID))
}

func (c *Cluster) podEvent(pod *corev1.Pod, typ, reason, msg string) {
	c.recordEvent(pod, "Pod", typ, reason, msg, "kubelet", pod.Spec.NodeName)
}

// missingVolumeSources returns FailedMount messages for missing config.
func (c *Cluster) missingVolumeSources(pod *corev1.Pod) []string {
	var out []string
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil && (v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional) {
			if _, err := c.Store.Get(gvrConfigMaps, pod.Namespace, v.ConfigMap.Name); err != nil {
				out = append(out, fmt.Sprintf("MountVolume.SetUp failed for volume %q : configmap %q not found", v.Name, v.ConfigMap.Name))
			}
		}
		if v.Secret != nil && (v.Secret.Optional == nil || !*v.Secret.Optional) {
			if _, err := c.Store.Get(gvrSecrets, pod.Namespace, v.Secret.SecretName); err != nil {
				out = append(out, fmt.Sprintf("MountVolume.SetUp failed for volume %q : secret %q not found", v.Name, v.Secret.SecretName))
			}
		}
		if v.PersistentVolumeClaim != nil {
			out = append(out, fmt.Sprintf("Unable to attach or mount volumes: unmounted volumes=[%s]: persistentvolumeclaim %q not found (PVCs are not simulated yet)", v.Name, v.PersistentVolumeClaim.ClaimName))
		}
	}
	return out
}

// resolveEnv returns the environment of a container or a config error
// formatted like the kubelet.
func (c *Cluster) resolveEnv(pod *corev1.Pod, ct *corev1.Container) (map[string]string, string) {
	env := map[string]string{}
	getCM := func(name string) (*corev1.ConfigMap, bool) {
		o, err := c.Store.Get(gvrConfigMaps, pod.Namespace, name)
		if err != nil {
			return nil, false
		}
		return o.(*corev1.ConfigMap), true
	}
	getSecret := func(name string) (*corev1.Secret, bool) {
		o, err := c.Store.Get(gvrSecrets, pod.Namespace, name)
		if err != nil {
			return nil, false
		}
		return o.(*corev1.Secret), true
	}
	for _, ef := range ct.EnvFrom {
		if ef.ConfigMapRef != nil {
			cm, ok := getCM(ef.ConfigMapRef.Name)
			if !ok {
				if ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional {
					continue
				}
				return nil, fmt.Sprintf("configmap %q not found", ef.ConfigMapRef.Name)
			}
			for k, v := range cm.Data {
				env[ef.Prefix+k] = v
			}
		}
		if ef.SecretRef != nil {
			s, ok := getSecret(ef.SecretRef.Name)
			if !ok {
				if ef.SecretRef.Optional != nil && *ef.SecretRef.Optional {
					continue
				}
				return nil, fmt.Sprintf("secret %q not found", ef.SecretRef.Name)
			}
			for k, v := range s.Data {
				env[ef.Prefix+k] = string(v)
			}
		}
	}
	for _, e := range ct.Env {
		switch {
		case e.ValueFrom == nil:
			env[e.Name] = e.Value
		case e.ValueFrom.ConfigMapKeyRef != nil:
			ref := e.ValueFrom.ConfigMapKeyRef
			optional := ref.Optional != nil && *ref.Optional
			cm, ok := getCM(ref.Name)
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Sprintf("configmap %q not found", ref.Name)
			}
			v, ok := cm.Data[ref.Key]
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Sprintf("couldn't find key %s in ConfigMap %s/%s", ref.Key, pod.Namespace, ref.Name)
			}
			env[e.Name] = v
		case e.ValueFrom.SecretKeyRef != nil:
			ref := e.ValueFrom.SecretKeyRef
			optional := ref.Optional != nil && *ref.Optional
			s, ok := getSecret(ref.Name)
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Sprintf("secret %q not found", ref.Name)
			}
			v, ok := s.Data[ref.Key]
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Sprintf("couldn't find key %s in Secret %s/%s", ref.Key, pod.Namespace, ref.Name)
			}
			env[e.Name] = string(v)
		default:
			env[e.Name] = "(simulated)"
		}
	}
	return env, ""
}

func pullDuration(p *ImageProfile) time.Duration {
	d := time.Second + time.Duration(p.SizeMB/60)*time.Second
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

func nextBackoff(cur int) int {
	if cur == 0 {
		return 10
	}
	if cur*2 > 300 {
		return 300
	}
	return cur * 2
}

func (c *Cluster) nodeHasImage(node, image string) bool {
	return slices.Contains(c.st.NodeImages[node], image)
}

func (c *Cluster) syncPod(pod *corev1.Pod, rt *podRT, now time.Time) {
	if msgs := c.missingVolumeSources(pod); len(msgs) > 0 {
		if !now.Before(rt.NextMount) {
			for _, m := range msgs {
				c.podEvent(pod, corev1.EventTypeWarning, "FailedMount", m)
			}
			rt.NextMount = now.Add(15 * time.Second)
		}
		c.writePodStatus(pod, rt, now)
		return
	}
	if rt.IP == "" {
		idx := c.nodeIndex(pod.Spec.NodeName)
		if pod.Spec.HostNetwork {
			for _, n := range DefaultNodes() {
				if n.Name == pod.Spec.NodeName {
					rt.IP = n.IP
				}
			}
		}
		if rt.IP == "" {
			c.st.NextPodIP[pod.Spec.NodeName]++
			rt.IP = fmt.Sprintf("10.244.%d.%d", idx, c.st.NextPodIP[pod.Spec.NodeName]+1)
		}
	}
	for i := range pod.Spec.Containers {
		c.syncContainer(pod, rt, &pod.Spec.Containers[i], now)
	}
	c.writePodStatus(pod, rt, now)
}

func (c *Cluster) syncContainer(pod *corev1.Pod, rt *podRT, spec *corev1.Container, now time.Time) {
	ct := rt.ctr(spec.Name)
	image := trimImage(spec.Image)
	if ct.Phase != cNew && ct.Image != image {
		// kubectl set image on a bare pod: the kubelet restarts the container.
		if ct.Phase == cRunning {
			c.podEvent(pod, corev1.EventTypeNormal, "Killing", fmt.Sprintf("Container %s definition changed, will be restarted", spec.Name))
		}
		ct.Phase = cNew
		ct.PullBackoff = 0
		ct.Ready = false
	}
	ct.Image = image
	for step := 0; step < 6; step++ {
		before := ct.Phase
		c.stepContainer(pod, rt, ct, spec, now)
		if ct.Phase == before {
			break
		}
	}
}

func (c *Cluster) stepContainer(pod *corev1.Pod, rt *podRT, ct *ctrRT, spec *corev1.Container, now time.Time) {
	node := pod.Spec.NodeName
	switch ct.Phase {
	case cNew:
		needPull := spec.ImagePullPolicy == corev1.PullAlways || !c.nodeHasImage(node, ct.Image)
		if spec.ImagePullPolicy == corev1.PullNever && !c.nodeHasImage(node, ct.Image) {
			ct.Phase = cPullFailed
			ct.PullError = "ErrImageNeverPull"
			c.podEvent(pod, corev1.EventTypeWarning, "ErrImageNeverPull", fmt.Sprintf("Container image %q is not present with pull policy of Never", ct.Image))
			ct.NextAt = now.Add(time.Hour * 24 * 365)
			return
		}
		if needPull {
			c.podEvent(pod, corev1.EventTypeNormal, "Pulling", fmt.Sprintf("Pulling image %q", ct.Image))
			ct.Phase = cPulling
			// Starts the registry lookup (if enabled) while the pull "runs".
			_ = c.decidePull(ct.Image)
			ct.NextAt = now.Add(pullDuration(profileOf(ct.Image)))
			return
		}
		c.podEvent(pod, corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Container image %q already present on machine", ct.Image))
		ct.Phase = cCreating
	case cPulling:
		if now.Before(ct.NextAt) {
			return
		}
		out := c.decidePull(ct.Image)
		if out.pending {
			// The registry has not answered yet; a real pull would still be
			// resolving the reference, too.
			ct.NextAt = now.Add(time.Second)
			return
		}
		if out.note != "" {
			c.podEvent(pod, corev1.EventTypeWarning, "RegistryUnreachable", out.note)
		}
		p, err := out.profile, out.err
		if err != nil {
			ct.Phase = cPullFailed
			ct.PullError = "ErrImagePull"
			ct.PullMessage = err.Error()
			c.podEvent(pod, corev1.EventTypeWarning, "Failed", fmt.Sprintf("Failed to pull image %q: %s", ct.Image, err.Error()))
			c.podEvent(pod, corev1.EventTypeWarning, "Failed", "Error: ErrImagePull")
			ct.NextAt = now.Add(2 * time.Second)
			return
		}
		d := pullDuration(p)
		c.podEvent(pod, corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Successfully pulled image %q in %.3fs (%.3fs including waiting). Image size: %d bytes.", ct.Image, d.Seconds(), d.Seconds(), p.SizeMB*1000*1000))
		if !c.nodeHasImage(node, ct.Image) {
			c.st.NodeImages[node] = append(c.st.NodeImages[node], ct.Image)
		}
		ct.PullBackoff = 0
		ct.PullError = ""
		ct.PullMessage = ""
		ct.ImageDigest = out.digest
		ct.Phase = cCreating
	case cPullFailed:
		if ct.PullError == "ErrImageNeverPull" || now.Before(ct.NextAt) {
			return
		}
		ct.PullBackoff = nextBackoff(ct.PullBackoff)
		ct.Phase = cPullBackOff
		c.podEvent(pod, corev1.EventTypeNormal, "BackOff", fmt.Sprintf("Back-off pulling image %q", ct.Image))
		c.podEvent(pod, corev1.EventTypeWarning, "Failed", "Error: ImagePullBackOff")
		ct.NextAt = now.Add(time.Duration(ct.PullBackoff) * time.Second)
	case cPullBackOff:
		if now.Before(ct.NextAt) {
			return
		}
		c.podEvent(pod, corev1.EventTypeNormal, "Pulling", fmt.Sprintf("Pulling image %q", ct.Image))
		ct.Phase = cPulling
		ct.NextAt = now.Add(time.Second)
	case cCreating:
		env, cfgErr := c.resolveEnv(pod, spec)
		if cfgErr != "" {
			if ct.ConfigError != cfgErr || !now.Before(ct.NextAt) {
				c.podEvent(pod, corev1.EventTypeWarning, "Failed", "Error: "+cfgErr)
				ct.NextAt = now.Add(10 * time.Second)
			}
			ct.ConfigError = cfgErr
			return
		}
		ct.ConfigError = ""
		c.startContainer(pod, ct, spec, env, now)
	case cRunning:
		if !ct.Plan.LongRunning && !now.Before(ct.StartedAt.Add(ct.Plan.RunFor)) {
			c.terminate(pod, ct, ct.Plan.ExitCode, ct.Plan.Reason, now)
			return
		}
		c.probe(pod, rt, ct, spec, now)
	case cTerminated:
		if now.Before(ct.NextAt) {
			return
		}
		ct.Restarts++
		c.podEvent(pod, corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Container image %q already present on machine", ct.Image))
		ct.Phase = cCreating
	case cDone:
	}
}

func (c *Cluster) startContainer(pod *corev1.Pod, ct *ctrRT, spec *corev1.Container, env map[string]string, now time.Time) {
	p := profileOf(ct.Image)
	ct.ContainerID = "containerd://" + c.hex(64)
	ref := ParseImage(ct.Image)
	digest := ct.ImageDigest
	if digest == "" {
		digest = c.knownDigest(ct.Image)
	}
	if digest == "" {
		digest = "sha256:" + fmt.Sprintf("%016x%016x%016x%016x", hash64(ref.Full()), hash64(ref.Full(), "1"), hash64(ref.Full(), "2"), hash64(ref.Full(), "3"))
	}
	ct.ImageID = ref.Repo() + "@" + digest
	c.podEvent(pod, corev1.EventTypeNormal, "Created", "Created container: "+spec.Name)
	c.podEvent(pod, corev1.EventTypeNormal, "Started", "Started container "+spec.Name)
	ct.Phase = cRunning
	ct.StartedAt = now
	ct.Plan = PlanRun(p, spec, env, now)
	if p == defaultProfile && ct.Plan.Ports != nil {
		ports := append([]int32{}, ct.Plan.Ports...)
		for _, cp := range spec.Ports {
			ports = append(ports, cp.ContainerPort)
		}
		ct.Plan.Ports = ports
	}
	ct.Logs = ct.Plan.Logs
	ct.Ready = spec.ReadinessProbe == nil
	ct.ProbeFails, ct.ProbeOK, ct.LiveFails = 0, 0, 0
	if spec.ReadinessProbe != nil {
		ct.NextProbe = now.Add(time.Duration(spec.ReadinessProbe.InitialDelaySeconds) * time.Second)
	}
	if spec.LivenessProbe != nil {
		ct.NextLive = now.Add(time.Duration(spec.LivenessProbe.InitialDelaySeconds) * time.Second)
	}
}

func (c *Cluster) terminate(pod *corev1.Pod, ct *ctrRT, exit int32, reason string, now time.Time) {
	if reason == "" {
		reason = "Completed"
		if exit != 0 {
			reason = "Error"
		}
	}
	ct.Term = &termInfo{ExitCode: exit, Reason: reason, StartedAt: ct.StartedAt, FinishedAt: now, ContainerID: ct.ContainerID}
	ct.PrevLogs = ct.Logs
	ct.Ready = false
	ct.ShowTermUntil = now.Add(2 * time.Second)
	restart := pod.Spec.RestartPolicy == corev1.RestartPolicyAlways ||
		(pod.Spec.RestartPolicy == corev1.RestartPolicyOnFailure && exit != 0)
	if !restart {
		ct.Phase = cDone
		return
	}
	ct.Phase = cTerminated
	if ct.Restarts == 0 {
		// Like the kubelet, the first restart happens almost immediately;
		// the exponential back-off starts with the second crash.
		ct.NextAt = now.Add(2 * time.Second)
	} else {
		ct.CrashBackoff = nextBackoff(ct.CrashBackoff)
		ct.NextAt = now.Add(time.Duration(ct.CrashBackoff) * time.Second)
	}
	c.podEvent(pod, corev1.EventTypeWarning, "BackOff", fmt.Sprintf("Back-off restarting failed container %s in pod %s_%s(%s)", ct.Name, pod.Name, pod.Namespace, pod.UID))
}

// probeResult evaluates a probe against the simulated process.
func (c *Cluster) probeResult(pr *corev1.Probe, rt *podRT, ct *ctrRT, spec *corev1.Container) (bool, string) {
	listening := func(port int32) bool { return slices.Contains(ct.Plan.Ports, port) }
	resolvePort := func(p intstr.IntOrString) int32 {
		if p.Type == intstr.Int {
			return p.IntVal
		}
		for _, cp := range spec.Ports {
			if cp.Name == p.StrVal {
				return cp.ContainerPort
			}
		}
		return 0
	}
	switch {
	case pr.HTTPGet != nil:
		port := resolvePort(pr.HTTPGet.Port)
		scheme := strings.ToLower(string(pr.HTTPGet.Scheme))
		url := fmt.Sprintf("%s://%s:%d%s", scheme, rt.IP, port, pr.HTTPGet.Path)
		if port == 0 {
			return false, fmt.Sprintf("Get %q: invalid port name %q", url, pr.HTTPGet.Port.StrVal)
		}
		if !listening(port) {
			return false, fmt.Sprintf("Get %q: dial tcp %s:%d: connect: connection refused", url, rt.IP, port)
		}
		if p := profileOf(ct.Image); len(p.HealthPaths) > 0 && !slices.Contains(p.HealthPaths, pr.HTTPGet.Path) {
			return false, "HTTP probe failed with statuscode: 404"
		}
		return true, ""
	case pr.TCPSocket != nil:
		port := resolvePort(pr.TCPSocket.Port)
		if !listening(port) {
			return false, fmt.Sprintf("dial tcp %s:%d: connect: connection refused", rt.IP, port)
		}
		return true, ""
	case pr.Exec != nil:
		cmd := strings.Join(pr.Exec.Command, " ")
		if strings.Contains(cmd, "false") || exitRe.MatchString(cmd) && !strings.Contains(cmd, "exit 0") || badWords.MatchString(cmd) {
			return false, fmt.Sprintf("command %q exited with 1", cmd)
		}
		return true, ""
	}
	return true, ""
}

func (c *Cluster) probe(pod *corev1.Pod, rt *podRT, ct *ctrRT, spec *corev1.Container, now time.Time) {
	if pr := spec.ReadinessProbe; pr != nil && !now.Before(ct.NextProbe) {
		ok, msg := c.probeResult(pr, rt, ct, spec)
		ct.NextProbe = now.Add(time.Duration(pr.PeriodSeconds) * time.Second)
		if ok {
			ct.ProbeFails = 0
			ct.ProbeOK++
			if ct.ProbeOK >= pr.SuccessThreshold {
				ct.Ready = true
			}
		} else {
			ct.ProbeOK = 0
			ct.ProbeFails++
			c.podEvent(pod, corev1.EventTypeWarning, "Unhealthy", "Readiness probe failed: "+msg)
			if ct.ProbeFails >= pr.FailureThreshold {
				ct.Ready = false
			}
		}
	}
	if pr := spec.LivenessProbe; pr != nil && !now.Before(ct.NextLive) {
		ok, msg := c.probeResult(pr, rt, ct, spec)
		ct.NextLive = now.Add(time.Duration(pr.PeriodSeconds) * time.Second)
		if ok {
			ct.LiveFails = 0
			return
		}
		ct.LiveFails++
		c.podEvent(pod, corev1.EventTypeWarning, "Unhealthy", "Liveness probe failed: "+msg)
		if ct.LiveFails >= pr.FailureThreshold {
			c.podEvent(pod, corev1.EventTypeNormal, "Killing", fmt.Sprintf("Container %s failed liveness probe, will be restarted", ct.Name))
			c.terminate(pod, ct, 137, "Error", now)
		}
	}
}

// writePodStatus renders the runtime state into pod.status.
func (c *Cluster) writePodStatus(pod *corev1.Pod, rt *podRT, now time.Time) {
	old := pod.Status.DeepCopy()
	st := corev1.PodStatus{QOSClass: old.QOSClass}
	if st.QOSClass == "" {
		st.QOSClass = qosClass(&pod.Spec)
	}
	start := metav1.NewTime(rt.StartTime)
	st.StartTime = &start
	for _, n := range DefaultNodes() {
		if n.Name == pod.Spec.NodeName {
			st.HostIP = n.IP
			st.HostIPs = []corev1.HostIP{{IP: n.IP}}
		}
	}
	if rt.IP != "" {
		st.PodIP = rt.IP
		st.PodIPs = []corev1.PodIP{{IP: rt.IP}}
	}
	allReady, anyRunning, allStarted := true, false, true
	allDone, anyFailed := true, false
	for _, spec := range pod.Spec.Containers {
		ct := rt.ctr(spec.Name)
		cs := corev1.ContainerStatus{Name: spec.Name, Image: spec.Image, ImageID: ct.ImageID, RestartCount: ct.Restarts, Ready: ct.Ready && ct.Phase == cRunning}
		started := ct.Phase == cRunning
		cs.Started = &started
		if ct.Term != nil {
			cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: ct.Term.ExitCode, Reason: ct.Term.Reason, StartedAt: metav1.NewTime(ct.Term.StartedAt),
				FinishedAt: metav1.NewTime(ct.Term.FinishedAt), ContainerID: ct.Term.ContainerID}}
		}
		switch ct.Phase {
		case cRunning:
			cs.ContainerID = ct.ContainerID
			cs.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(ct.StartedAt)}
			anyRunning = true
			allDone = false
		case cTerminated, cDone:
			cs.ContainerID = ct.ContainerID
			if ct.Phase == cDone || now.Before(ct.ShowTermUntil) {
				t := ct.Term
				cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: t.ExitCode, Reason: t.Reason,
					StartedAt: metav1.NewTime(t.StartedAt), FinishedAt: metav1.NewTime(t.FinishedAt), ContainerID: t.ContainerID}
				cs.LastTerminationState = corev1.ContainerState{}
			} else {
				cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff",
					Message: fmt.Sprintf("back-off %ds restarting failed container=%s pod=%s_%s(%s)", max(ct.CrashBackoff, 10), spec.Name, pod.Name, pod.Namespace, pod.UID)}
			}
			if ct.Phase == cTerminated {
				anyRunning = true // pod stays Running while restarting
				allDone = false
			} else if ct.Term.ExitCode != 0 {
				anyFailed = true
			}
		default:
			allDone = false
			allStarted = false
			w := &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
			switch ct.Phase {
			case cPullFailed:
				w.Reason = ct.PullError
				if ct.PullError == "ErrImagePull" {
					w.Message = ct.PullMessage
					if w.Message == "" { // state saved by an older version
						if _, err := ResolveImage(ct.Image); err != nil {
							w.Message = err.Error()
						}
					}
				} else {
					w.Message = fmt.Sprintf("Container image %q is not present with pull policy of Never", ct.Image)
				}
			case cPullBackOff:
				w.Reason = "ImagePullBackOff"
				w.Message = fmt.Sprintf("Back-off pulling image %q", ct.Image)
			case cCreating:
				if ct.ConfigError != "" {
					w.Reason = "CreateContainerConfigError"
					w.Message = ct.ConfigError
				}
			}
			cs.State.Waiting = w
		}
		if ct.Restarts > 0 || ct.Term != nil {
			allStarted = true
		}
		if !cs.Ready {
			allReady = false
		}
		st.ContainerStatuses = append(st.ContainerStatuses, cs)
	}
	switch {
	case allDone && len(pod.Spec.Containers) > 0:
		if anyFailed {
			st.Phase = corev1.PodFailed
		} else {
			st.Phase = corev1.PodSucceeded
		}
	case anyRunning && allStarted:
		st.Phase = corev1.PodRunning
	default:
		st.Phase = corev1.PodPending
	}
	cond := func(t corev1.PodConditionType, ok bool, reason, msg string) corev1.PodCondition {
		s := corev1.ConditionFalse
		if ok {
			s = corev1.ConditionTrue
			reason, msg = "", ""
		}
		pc := corev1.PodCondition{Type: t, Status: s, Reason: reason, Message: msg, LastTransitionTime: metav1.NewTime(now)}
		for _, oc := range old.Conditions {
			if oc.Type == t && oc.Status == s {
				pc.LastTransitionTime = oc.LastTransitionTime
			}
		}
		return pc
	}
	var notReady []string
	for _, cs := range st.ContainerStatuses {
		if !cs.Ready {
			notReady = append(notReady, cs.Name)
		}
	}
	nr := "containers with unready status: [" + strings.Join(notReady, " ") + "]"
	readyReason := "ContainersNotReady"
	if st.Phase == corev1.PodSucceeded || st.Phase == corev1.PodFailed {
		readyReason = "PodCompleted"
		nr = ""
	}
	st.Conditions = []corev1.PodCondition{
		cond(corev1.PodReadyToStartContainers, rt.IP != "", "", ""),
		cond(corev1.PodInitialized, true, "", ""),
		cond(corev1.PodReady, allReady && st.Phase == corev1.PodRunning, readyReason, nr),
		cond(corev1.ContainersReady, allReady && st.Phase == corev1.PodRunning, readyReason, nr),
		cond(corev1.PodScheduled, true, "", ""),
	}
	pod.Status = st
	_, _ = c.Store.Update(gvrPods, pod)
}
