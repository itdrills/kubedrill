package sim

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const scheduleRetry = 5 * time.Second

func podRequests(spec *corev1.PodSpec) (cpu, mem int64) {
	for _, ct := range spec.Containers {
		if q, ok := ct.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu += q.MilliValue()
		} else if q, ok := ct.Resources.Limits[corev1.ResourceCPU]; ok {
			cpu += q.MilliValue()
		}
		if q, ok := ct.Resources.Requests[corev1.ResourceMemory]; ok {
			mem += q.Value()
		} else if q, ok := ct.Resources.Limits[corev1.ResourceMemory]; ok {
			mem += q.Value()
		}
	}
	return
}

func tolerates(tols []corev1.Toleration, t corev1.Taint) bool {
	for _, tol := range tols {
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		if tol.Operator == corev1.TolerationOpExists && (tol.Key == "" || tol.Key == t.Key) {
			return true
		}
		if tol.Key == t.Key && (tol.Operator == "" || tol.Operator == corev1.TolerationOpEqual) && tol.Value == t.Value {
			return true
		}
	}
	return false
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func activePod(p *corev1.Pod) bool {
	return p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

// syncScheduler binds pending pods to nodes using filters (taints, node
// selector, resources) and a least-allocated score.
func (c *Cluster) syncScheduler() {
	now := c.Clock.Now()
	nodes := listTyped[*corev1.Node](c, gvrNodes, "")
	pods := listTyped[*corev1.Pod](c, gvrPods, "")
	usedCPU := map[string]int64{}
	usedMem := map[string]int64{}
	count := map[string]int{}
	for _, p := range pods {
		if p.Spec.NodeName != "" && activePod(p) {
			cpu, mem := podRequests(&p.Spec)
			usedCPU[p.Spec.NodeName] += cpu
			usedMem[p.Spec.NodeName] += mem
			count[p.Spec.NodeName]++
		}
	}
	for _, p := range pods {
		if p.Spec.NodeName != "" || p.Spec.SchedulerName != corev1.DefaultSchedulerName {
			continue
		}
		if last, ok := c.st.Scheduled[string(p.UID)]; ok && now.Before(last.Add(scheduleRetry)) {
			continue
		}
		cpu, mem := podRequests(&p.Spec)
		reasons := map[string]int{}
		var feasible []*corev1.Node
		for _, n := range nodes {
			var why []string
			if n.Spec.Unschedulable {
				why = append(why, "node(s) were unschedulable")
			}
			taints := append([]corev1.Taint{}, n.Spec.Taints...)
			if !nodeReady(n) {
				taints = append(taints, corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule})
			}
			for _, t := range taints {
				if (t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute) && !tolerates(p.Spec.Tolerations, t) {
					why = append(why, fmt.Sprintf("node(s) had untolerated taint {%s: %s}", t.Key, t.Value))
					break
				}
			}
			for k, v := range p.Spec.NodeSelector {
				if n.Labels[k] != v {
					why = append(why, "node(s) didn't match Pod's node affinity/selector")
					break
				}
			}
			if len(why) == 0 {
				alloc := n.Status.Allocatable
				ac := alloc[corev1.ResourceCPU]
				am := alloc[corev1.ResourceMemory]
				ap := alloc[corev1.ResourcePods]
				if count[n.Name]+1 > int(ap.Value()) {
					why = append(why, "Too many pods")
				}
				if usedCPU[n.Name]+cpu > ac.MilliValue() {
					why = append(why, "Insufficient cpu")
				}
				if usedMem[n.Name]+mem > am.Value() {
					why = append(why, "Insufficient memory")
				}
			}
			if len(why) == 0 {
				feasible = append(feasible, n)
			}
			for _, w := range why {
				reasons[w]++
			}
		}
		if len(feasible) == 0 {
			c.st.Scheduled[string(p.UID)] = now
			var parts []string
			for r, n := range reasons {
				parts = append(parts, fmt.Sprintf("%d %s", n, r))
			}
			sort.Strings(parts)
			preemptHelpful := 0
			for r, n := range reasons {
				if strings.HasPrefix(r, "Insufficient") {
					preemptHelpful += n
				}
			}
			msg := fmt.Sprintf("0/%d nodes are available: %s. preemption: 0/%d nodes are available: ", len(nodes), strings.Join(parts, ", "), len(nodes))
			notHelpful := len(nodes) - preemptHelpful
			var pp []string
			if notHelpful > 0 {
				pp = append(pp, fmt.Sprintf("%d Preemption is not helpful for scheduling", notHelpful))
			}
			if preemptHelpful > 0 {
				pp = append(pp, fmt.Sprintf("%d No preemption victims found for incoming pod", preemptHelpful))
			}
			msg += strings.Join(pp, ", ") + "."
			c.recordEvent(p, "Pod", corev1.EventTypeWarning, "FailedScheduling", msg, "default-scheduler", "")
			cond := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: msg, LastTransitionTime: metav1.NewTime(now)}
			if len(p.Status.Conditions) == 1 && p.Status.Conditions[0].Message == msg {
				cond.LastTransitionTime = p.Status.Conditions[0].LastTransitionTime
			}
			p.Status.Conditions = []corev1.PodCondition{cond}
			p.Status.Phase = corev1.PodPending
			_, _ = c.Store.Update(gvrPods, p)
			continue
		}
		best := feasible[0]
		bestScore := -1.0
		for _, n := range feasible {
			ac := n.Status.Allocatable[corev1.ResourceCPU]
			am := n.Status.Allocatable[corev1.ResourceMemory]
			score := freeFrac(ac, usedCPU[n.Name]+cpu, true) + freeFrac(am, usedMem[n.Name]+mem, false)
			if score > bestScore+1e-9 {
				best, bestScore = n, score
			}
		}
		delete(c.st.Scheduled, string(p.UID))
		p.Spec.NodeName = best.Name
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
		if _, err := c.Store.Update(gvrPods, p); err == nil {
			usedCPU[best.Name] += cpu
			usedMem[best.Name] += mem
			count[best.Name]++
			c.recordEvent(p, "Pod", corev1.EventTypeNormal, "Scheduled", fmt.Sprintf("Successfully assigned %s/%s to %s", p.Namespace, p.Name, best.Name), "default-scheduler", "")
		}
	}
}

func freeFrac(alloc resource.Quantity, used int64, milli bool) float64 {
	total := alloc.Value()
	if milli {
		total = alloc.MilliValue()
	}
	if total == 0 {
		return 0
	}
	return float64(total-used) / float64(total)
}
