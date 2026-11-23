package sim

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Logs returns synthetic logs of a container. Errors mirror the kubelet's
// messages so learners see what a real cluster would print.
func (c *Cluster) Logs(ns, podName, container string, previous bool) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, err := c.Store.Get(gvrPods, ns, podName)
	if err != nil {
		return nil, err
	}
	pod := o.(*corev1.Pod)
	if container == "" {
		if len(pod.Spec.Containers) > 1 {
			var names []string
			for _, ct := range pod.Spec.Containers {
				names = append(names, ct.Name)
			}
			return nil, apierrors.NewBadRequest(fmt.Sprintf("a container name must be specified for pod %s, choose one of: [%s]", pod.Name, strings.Join(names, " ")))
		}
		container = pod.Spec.Containers[0].Name
	}
	found := false
	for _, ct := range pod.Spec.Containers {
		if ct.Name == container {
			found = true
		}
	}
	if !found {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("container %s is not valid for pod %s", container, pod.Name))
	}
	rt := c.st.Pods[string(pod.UID)]
	var ct *ctrRT
	if rt != nil {
		for _, x := range rt.Containers {
			if x.Name == container {
				ct = x
			}
		}
	}
	if previous {
		if ct == nil || ct.Term == nil {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("previous terminated container %q in pod %q not found", container, pod.Name))
		}
		return ct.PrevLogs, nil
	}
	if ct == nil || ct.Phase == cNew || ct.Phase == cPulling || ct.Phase == cPullFailed || ct.Phase == cPullBackOff || (ct.Phase == cCreating && ct.Term == nil) {
		reason := "ContainerCreating"
		if ct != nil {
			switch ct.Phase {
			case cPullFailed, cPullBackOff:
				reason = "trying and failing to pull image"
			case cCreating:
				if ct.ConfigError != "" {
					reason = "CreateContainerConfigError: " + ct.ConfigError
				}
			}
		}
		return nil, apierrors.NewBadRequest(fmt.Sprintf("container %q in pod %q is waiting to start: %s", container, pod.Name, reason))
	}
	if ct.Phase == cTerminated || ct.Phase == cCreating {
		// While in CrashLoopBackOff the "current" logs are those of the last run.
		return ct.PrevLogs, nil
	}
	return ct.Logs, nil
}
