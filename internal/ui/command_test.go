package ui

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAccepts(t *testing.T) {
	cases := map[string][]string{
		"kubectl get pods":                                            {"get", "pods"},
		"  kubectl   get pods -A  ":                                   {"get", "pods", "-A"},
		`kubectl get deploy web -o jsonpath={.spec.replicas}`:         {"get", "deploy", "web", "-o", "jsonpath={.spec.replicas}"},
		`kubectl get pods -o 'jsonpath={.items[*].metadata.name}'`:    {"get", "pods", "-o", "jsonpath={.items[*].metadata.name}"},
		`kubectl create configmap app --from-literal="mode=demo app"`: {"create", "configmap", "app", "--from-literal=mode=demo app"},
		`kubectl set image deployment/web web=nginx:1.27`:             {"set", "image", "deployment/web", "web=nginx:1.27"},
		"kubectl -n kube-system get pods":                             {"-n", "kube-system", "get", "pods"},
		"kubectl apply -f app.yaml":                                   {"apply", "-f", "app.yaml"},
		`kubectl label pod web a\ b=c`:                                {"label", "pod", "web", "a b=c"},
	}
	for line, want := range cases {
		cmd, err := Parse(line)
		if err != nil {
			t.Errorf("Parse(%q): %v", line, err)
			continue
		}
		if cmd.Program != Kubectl || !reflect.DeepEqual(cmd.Args, want) {
			t.Errorf("Parse(%q) = %s %q, want kubectl %q", line, cmd.Program, cmd.Args, want)
		}
	}
	for _, line := range []string{"kubedrill hint", "kubedrill solve", "kubedrill status", "kubedrill challenge list",
		"kubedrill challenge start image-pull-backoff", "kubedrill challenge check", "kubedrill challenge stop --keep"} {
		cmd, err := Parse(line)
		if err != nil || cmd.Program != Kubedrill {
			t.Errorf("Parse(%q) = %v, %v", line, cmd, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"":                                   "empty",
		"ls -la":                             "not a shell",
		"bash -c id":                         "not a shell",
		"/usr/bin/kubectl get pods":          "not a shell",
		"kubectl get pods | grep web":        "shell feature",
		"kubectl get pods; rm -rf ~":         "shell feature",
		"kubectl get pods && id":             "shell feature",
		"kubectl get pods > out.txt":         "shell feature",
		"kubectl get $(id)":                  "shell feature",
		"kubectl get `id`":                   "shell feature",
		`kubectl get "$HOME"`:                "substitution",
		"kubectl get pods\nid":               "shell feature",
		"kubectl 'get pods":                  "unterminated",
		"kubectl":                            "which kubectl command",
		"kubectl exec web -- sh":             "not available",
		"kubectl port-forward svc/web 8080":  "not available",
		"kubectl proxy":                      "not available",
		"kubectl config view --raw":          "not available",
		"kubectl debug node/x":               "not available",
		"kubectl edit deploy web":            "not available",
		"kubectl myplugin":                   "not available",
		"kubectl --context prod get pods":    "--context",
		"kubectl get pods --context=prod":    "--context",
		"kubectl get pods --kubeconfig x":    "--kubeconfig",
		"kubectl get pods -s https://x":      "-s",
		"kubectl get pods --token=abc":       "--token",
		"kubectl get pods -w":                "-w",
		"kubectl get pods --watch":           "--watch",
		"kubectl logs web -f":                "follow",
		"kubectl logs web --follow":          "--follow",
		"kubectl apply -f -":                 "stdin",
		"kubedrill":                          "challenge list",
		"kubedrill start":                    "not available",
		"kubedrill stop":                     "not available",
		"kubedrill reset":                    "not available",
		"kubedrill ui":                       "not available",
		"kubedrill kubectl get pods":         "not available",
		"kubedrill kubeconfig remove":        "not available",
		"kubedrill challenge validate /etc":  "list|start",
		"kubedrill challenge":                "list|start",
		strings.Repeat("a", MaxLineLength+1): "too long",
	}
	for line, want := range cases {
		_, err := Parse(line)
		if err == nil {
			t.Errorf("Parse(%q) accepted, want error containing %q", line, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error %q does not contain %q", line, err, want)
		}
	}
}

func TestParseFindsVerbAfterGlobalFlags(t *testing.T) {
	// "get" is the namespace here; the real subcommand is a plugin name.
	if _, err := Parse("kubectl -n get myplugin"); err == nil || !strings.Contains(err.Error(), "myplugin") {
		t.Fatalf("plugin behind namespace value accepted: %v", err)
	}
	if _, err := Parse("kubectl --insecure get pods"); err == nil {
		t.Fatal("unknown leading flag accepted")
	}
	if _, err := Parse("kubectl --namespace=kube-system get pods"); err != nil {
		t.Fatal(err)
	}
}
