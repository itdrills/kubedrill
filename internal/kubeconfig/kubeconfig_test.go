package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const userConfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://prod.example.invalid:6443
  name: prod
contexts:
- context:
    cluster: prod
    user: admin
  name: prod
current-context: prod
users:
- name: admin
  user:
    token: secret-prod-token
`

func setup(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", p)
	return p
}

func TestMergePreservesExistingContexts(t *testing.T) {
	p := setup(t, userConfig)
	if _, err := Merge(Entry{Server: "https://127.0.0.1:16443", Token: "tok", CAData: []byte("ca")}, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.LoadFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "prod" {
		t.Fatalf("current-context changed to %q", cfg.CurrentContext)
	}
	if cfg.Clusters["prod"].Server != "https://prod.example.invalid:6443" || cfg.AuthInfos["admin"].Token != "secret-prod-token" {
		t.Fatal("existing prod entries were modified")
	}
	if cfg.Clusters["kubedrill"].Server != "https://127.0.0.1:16443" || cfg.Contexts["kubedrill"] == nil {
		t.Fatal("kubedrill entries missing")
	}
	// Remove restores the original entry set.
	if removed, err := Remove(); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	cfg, _ = clientcmd.LoadFromFile(p)
	if _, ok := cfg.Contexts["kubedrill"]; ok || cfg.CurrentContext != "prod" || len(cfg.Contexts) != 1 {
		t.Fatalf("unexpected config after remove: %+v", cfg.Contexts)
	}
}

func TestMergeSetCurrentIsOptIn(t *testing.T) {
	p := setup(t, userConfig)
	if _, err := Merge(Entry{Server: "https://127.0.0.1:16443", Token: "tok"}, true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := clientcmd.LoadFromFile(p)
	if cfg.CurrentContext != "kubedrill" {
		t.Fatalf("expected opt-in switch, got %q", cfg.CurrentContext)
	}
}

func TestRefusesForeignKubedrillContext(t *testing.T) {
	p := setup(t, strings.ReplaceAll(userConfig, "name: prod\ncontexts", "name: kubedrill\ncontexts"))
	before, _ := os.ReadFile(p)
	_, err := Merge(Entry{Server: "https://127.0.0.1:16443", Token: "tok"}, false)
	var fe *ErrForeignContext
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}
	_ = fe
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("kubeconfig was modified despite refusal")
	}
}

func TestIsLoopback(t *testing.T) {
	for in, want := range map[string]bool{
		"https://127.0.0.1:16443": true, "https://localhost:1": true, "https://[::1]:2": true,
		"https://10.0.0.1:6443": false, "https://prod.example.com": false,
	} {
		if IsLoopback(in) != want {
			t.Errorf("IsLoopback(%s) != %v", in, want)
		}
	}
}
