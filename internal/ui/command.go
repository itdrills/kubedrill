package ui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/itdrills/kubedrill/internal/brand"
)

// Program names the binary a validated command line runs.
type Program string

// The only two programs the browser terminal may run.
const (
	Kubectl   Program = "kubectl"
	Kubedrill Program = brand.Binary
)

// Command is a validated command line from the browser terminal.
type Command struct {
	Program Program
	Args    []string // without the program name
}

// MaxLineLength bounds a single command line.
const MaxLineLength = 4096

// kubectlVerbs are the kubectl subcommands the browser terminal accepts. The
// list is an allowlist on purpose: unknown subcommands would make kubectl look
// for plugins (kubectl-<name>) in PATH and execute them.
var kubectlVerbs = map[string]bool{
	"get": true, "describe": true, "logs": true, "explain": true, "api-resources": true,
	"api-versions": true, "version": true, "events": true, "cluster-info": true,
	"create": true, "apply": true, "delete": true, "replace": true, "patch": true,
	"label": true, "annotate": true, "set": true, "scale": true, "rollout": true,
	"expose": true, "run": true, "taint": true, "cordon": true, "uncordon": true,
	"drain": true, "wait": true,
}

// kubectlBlockedFlags would point kubectl at another cluster, wait forever
// (the terminal is not interactive) or read from stdin.
var kubectlBlockedFlags = []string{
	"--context", "--kubeconfig", "--cluster", "--server", "-s", "--user", "--token", "--as",
	"--as-group", "--as-uid", "--certificate-authority", "--client-certificate", "--client-key",
	"--insecure-skip-tls-verify", "--tls-server-name", "--username", "--password",
	"--watch", "-w", "--watch-only", "--follow",
}

// kubedrillCommands are the kubedrill subcommands the browser terminal accepts;
// the value lists the allowed second word (nil = no second word required).
var kubedrillCommands = map[string]map[string]bool{
	"challenge": {"list": true, "start": true, "status": true, "check": true, "reset": true, "stop": true},
	"hint":      nil,
	"solve":     nil,
	"status":    nil,
}

// Parse splits a command line like a POSIX shell would for plain words, single
// and double quotes and backslash escapes. It never expands variables, globs or
// substitutions and rejects shell operators, so nothing can reach a shell.
func Parse(line string) (*Command, error) {
	if len(line) > MaxLineLength {
		return nil, fmt.Errorf("command too long (max. %d characters)", MaxLineLength)
	}
	words, err := split(line)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, errors.New("empty command")
	}
	cmd := &Command{Program: Program(words[0]), Args: words[1:]}
	switch cmd.Program {
	case Kubectl:
		return cmd, validateKubectl(cmd.Args)
	case Kubedrill:
		return cmd, validateKubedrill(cmd.Args)
	default:
		return nil, fmt.Errorf("only `kubectl ...` and `%s challenge|hint|solve|status` run here – this is not a shell", brand.Binary)
	}
}

func split(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			case '$', '`':
				return nil, errors.New("variables and command substitution are not supported")
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		// Pipes, redirects, chaining and substitutions would need a shell. Globs and
		// braces are passed through literally (e.g. -o jsonpath={.status.phase}).
		case strings.ContainsRune("|&;<>()$`\n\r", r):
			return nil, fmt.Errorf("%q is a shell feature and not supported here – this is not a shell", r)
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func validateKubectl(args []string) error {
	for _, a := range args {
		for _, f := range kubectlBlockedFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("%s is not allowed in the browser terminal", f)
			}
		}
	}
	// Find the subcommand exactly like kubectl does: skip global flags before it.
	// Only a few well-known global flags may come first; with anything else the
	// subcommand could not be determined reliably, so it is rejected.
	verb := ""
	for i := 0; i < len(args) && verb == ""; i++ {
		a := args[i]
		switch {
		case !strings.HasPrefix(a, "-"):
			verb = a
		case a == "-n" || a == "--namespace" || a == "-v" || a == "--v" || a == "--request-timeout":
			i++ // value follows
		case strings.HasPrefix(a, "--namespace=") || strings.HasPrefix(a, "-n=") || strings.HasPrefix(a, "--v=") ||
			strings.HasPrefix(a, "-v=") || strings.HasPrefix(a, "--request-timeout=") || a == "-A" || a == "--all-namespaces":
		default:
			return fmt.Errorf("put %s after the kubectl command (e.g. `kubectl get pods %s ...`)", a, a)
		}
	}
	if verb == "" {
		return errors.New("which kubectl command? e.g. `kubectl get pods`")
	}
	if !kubectlVerbs[verb] {
		return fmt.Errorf("`kubectl %s` is not available in the browser terminal; use your own terminal for it", verb)
	}
	if verb == "logs" {
		for _, a := range args {
			if a == "-f" || strings.HasPrefix(a, "-f=") {
				return errors.New("-f (follow) is not available in the browser terminal")
			}
		}
	}
	for i, a := range args {
		// "-f -" reads a manifest from stdin, which the browser terminal does not have.
		if (a == "-f" || a == "--filename") && i+1 < len(args) && args[i+1] == "-" {
			return errors.New("reading from stdin (-f -) is not available in the browser terminal")
		}
	}
	return nil
}

func validateKubedrill(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("try `%s challenge list`", brand.Binary)
	}
	sub, ok := kubedrillCommands[args[0]]
	if !ok {
		return fmt.Errorf("`%s %s` is not available in the browser terminal", brand.Binary, args[0])
	}
	if sub != nil && (len(args) < 2 || !sub[args[1]]) {
		return fmt.Errorf("try `%s %s list|start|status|check|reset|stop`", brand.Binary, args[0])
	}
	return nil
}
