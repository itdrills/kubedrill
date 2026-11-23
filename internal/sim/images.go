package sim

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ImageProfile describes how a simulated image behaves. No image is ever
// downloaded or executed; the profile drives the simulated container.
type ImageProfile struct {
	// Repo is the normalised repository path, e.g. "docker.io/library/nginx".
	Repo string
	// Tags lists valid tags as a regular expression.
	Tags *regexp.Regexp
	// SizeMB is shown in "Pulled" events.
	SizeMB int
	// Ports the simulated process listens on.
	Ports []int32
	// HealthPaths that answer HTTP 200; empty means every path answers 200.
	HealthPaths []string
	// RequiredEnv variables; if one is missing the process exits with code 1.
	RequiredEnv []string
	// MemoryMB is the simulated working set; a lower memory limit causes OOMKilled.
	MemoryMB int64
	// Shell images interpret command/args (sleep, exit N, ...).
	Shell bool
	// Logs returns synthetic startup log lines.
	Logs func(c *corev1.Container, t time.Time) []string
	// Simulated images exist only inside the simulator (kubedrill/*); they are
	// never looked up in a real registry.
	Simulated bool
}

var semverish = `(\d+)(\.\d+){0,2}`

var catalog = []*ImageProfile{
	{Repo: "docker.io/library/nginx", Tags: regexp.MustCompile(`^(latest|stable|mainline|alpine|stable-alpine|mainline-alpine|perl|1\.(1[0-9]|2[0-9])(\.\d{1,2})?(-alpine|-perl|-bookworm|-alpine-slim)?)$`),
		SizeMB: 72, Ports: []int32{80}, MemoryMB: 12, Logs: nginxLogs},
	{Repo: "docker.io/library/httpd", Tags: regexp.MustCompile(`^(latest|alpine|2(\.4(\.\d{1,2})?)?(-alpine)?)$`), SizeMB: 58, Ports: []int32{80}, MemoryMB: 20, Logs: genericServerLogs("httpd", 80)},
	{Repo: "docker.io/library/redis", Tags: regexp.MustCompile(`^(latest|alpine|` + semverish + `(-alpine|-bookworm)?)$`), SizeMB: 45, Ports: []int32{6379}, MemoryMB: 8, Logs: redisLogs},
	{Repo: "docker.io/library/postgres", Tags: regexp.MustCompile(`^(latest|alpine|` + semverish + `(-alpine|-bookworm)?)$`), SizeMB: 160, Ports: []int32{5432}, MemoryMB: 64, RequiredEnv: []string{"POSTGRES_PASSWORD"}, Logs: postgresLogs},
	{Repo: "docker.io/library/mysql", Tags: regexp.MustCompile(`^(latest|lts|` + semverish + `(-oracle|-debian)?)$`), SizeMB: 170, Ports: []int32{3306}, MemoryMB: 360, RequiredEnv: []string{"MYSQL_ROOT_PASSWORD"}, Logs: genericServerLogs("mysqld", 3306)},
	{Repo: "docker.io/library/busybox", Tags: regexp.MustCompile(`^(latest|stable|musl|glibc|uclibc|1\.\d{2}(\.\d)?(-musl|-glibc|-uclibc)?)$`), SizeMB: 2, MemoryMB: 1, Shell: true},
	{Repo: "docker.io/library/alpine", Tags: regexp.MustCompile(`^(latest|edge|3(\.\d{1,2}){0,2})$`), SizeMB: 4, MemoryMB: 1, Shell: true},
	{Repo: "docker.io/library/ubuntu", Tags: regexp.MustCompile(`^(latest|rolling|devel|jammy|noble|2[0-9]\.(04|10))$`), SizeMB: 29, MemoryMB: 2, Shell: true},
	{Repo: "docker.io/library/debian", Tags: regexp.MustCompile(`^(latest|stable|bookworm|trixie|bookworm-slim|trixie-slim|1[0-9](\.\d+)?)$`), SizeMB: 49, MemoryMB: 2, Shell: true},
	{Repo: "docker.io/library/python", Tags: regexp.MustCompile(`^(latest|alpine|slim|3(\.\d{1,2}){0,2}(-alpine|-slim|-bookworm)?)$`), SizeMB: 380, MemoryMB: 20, Shell: true},
	{Repo: "docker.io/kubedrill/demo-api", Tags: regexp.MustCompile(`^(latest|1\.[0-9](\.[0-9])?)$`), SizeMB: 18, Ports: []int32{8080}, HealthPaths: []string{"/healthz", "/ready", "/"}, MemoryMB: 48, RequiredEnv: []string{"DATABASE_URL"}, Logs: demoAPILogs, Simulated: true},
	{Repo: "docker.io/kubedrill/memory-hog", Tags: regexp.MustCompile(`^(latest|1\.0)$`), SizeMB: 6, Ports: []int32{8080}, MemoryMB: 700, Logs: genericServerLogs("memory-hog", 8080), Simulated: true},
	{Repo: "registry.k8s.io/coredns/coredns", Tags: regexp.MustCompile(`^v1\.\d{1,2}\.\d{1,2}$`), SizeMB: 20, Ports: []int32{53, 8080, 8181, 9153}, MemoryMB: 15, Logs: genericServerLogs("coredns", 53)},
}

// defaultProfile is used for images not in the catalog (assumed to exist,
// e.g. private registry images used in custom scenarios).
var defaultProfile = &ImageProfile{SizeMB: 25, Ports: []int32{80, 8080}, MemoryMB: 16}

// ImageRef is a parsed image reference.
type ImageRef struct {
	Registry, Path, Tag, Digest string
}

// Full returns the fully qualified reference as containerd prints it.
func (r ImageRef) Full() string {
	s := r.Registry + "/" + r.Path
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	return s + ":" + r.Tag
}

// Repo returns registry/path.
func (r ImageRef) Repo() string { return r.Registry + "/" + r.Path }

// ParseImage normalises an image reference the way the container runtime does.
func ParseImage(image string) ImageRef {
	ref := ImageRef{}
	name := image
	if i := strings.Index(name, "@"); i >= 0 {
		ref.Digest = name[i+1:]
		name = name[:i]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i:], "/") {
		ref.Tag = name[i+1:]
		name = name[:i]
	}
	parts := strings.SplitN(name, "/", 2)
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		ref.Registry, ref.Path = parts[0], parts[1]
	} else {
		ref.Registry, ref.Path = "docker.io", name
	}
	if ref.Registry == "docker.io" && !strings.Contains(ref.Path, "/") {
		ref.Path = "library/" + ref.Path
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	return ref
}

// PullError explains why a simulated pull fails.
type PullError struct {
	Message string
}

func (e *PullError) Error() string { return e.Message }

var badWords = regexp.MustCompile(`(?i)(doesnotexist|does-not-exist|nonexistent|non-existent|invalid|notfound|typo)`)

// Reference returns the tag or, if set, the digest of the reference.
func (r ImageRef) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

func errImageNotFound(ref ImageRef) error {
	return &PullError{Message: fmt.Sprintf("rpc error: code = NotFound desc = failed to pull and unpack image %q: failed to resolve reference %q: %s: not found", ref.Full(), ref.Full(), ref.Full())}
}

func errImageAccessDenied(ref ImageRef) error {
	return &PullError{Message: fmt.Sprintf("failed to pull and unpack image %q: failed to resolve reference %q: pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed", ref.Full(), ref.Full())}
}

// catalogProfile returns the catalog entry of the repository (ignoring the
// tag) or nil.
func catalogProfile(ref ImageRef) *ImageProfile {
	for _, p := range catalog {
		if p.Repo == ref.Repo() {
			return p
		}
	}
	return nil
}

// profileOf returns how a (successfully pulled) image behaves: the catalog
// profile of its repository or a generic one.
func profileOf(image string) *ImageProfile {
	if p := catalogProfile(ParseImage(image)); p != nil {
		return p
	}
	return defaultProfile
}

// ResolveImage is the offline decision whether an image can be pulled: it
// uses the built-in catalog and typo heuristics. It returns the profile of an
// image or a PullError.
func ResolveImage(image string) (*ImageProfile, error) {
	if strings.TrimSpace(image) == "" {
		return nil, &PullError{Message: "InvalidImageName"}
	}
	ref := ParseImage(image)
	if p := catalogProfile(ref); p != nil {
		if ref.Digest != "" || p.Tags.MatchString(ref.Tag) {
			return p, nil
		}
		return nil, errImageNotFound(ref)
	}
	if badWords.MatchString(ref.Path) {
		return nil, errImageAccessDenied(ref)
	}
	if badWords.MatchString(ref.Tag) {
		return nil, errImageNotFound(ref)
	}
	// Typos of well known Docker Hub images ("ngnix", "nginxx", "busybx").
	if ref.Registry == "docker.io" {
		name := strings.TrimPrefix(ref.Path, "library/")
		for _, p := range catalog {
			known := strings.TrimPrefix(p.Repo, "docker.io/library/")
			if known == p.Repo || known == name {
				continue
			}
			if d := levenshtein(name, known); d > 0 && d <= 2 && len(name) >= 4 {
				return nil, errImageAccessDenied(ref)
			}
		}
	}
	return defaultProfile, nil
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// RunPlan is the simulated outcome of starting a container.
type RunPlan struct {
	// LongRunning processes keep running until stopped.
	LongRunning bool
	// ExitCode when the process terminates (LongRunning == false).
	ExitCode int32
	// Reason of termination ("Error", "Completed", "OOMKilled").
	Reason string
	// RunFor is how long the process runs before exiting.
	RunFor time.Duration
	// Logs emitted by the run.
	Logs []string
	// Ports the process listens on.
	Ports []int32
}

var exitRe = regexp.MustCompile(`exit\s+(\d+)`)

// PlanRun decides how a container behaves given its spec and resolved env.
func PlanRun(p *ImageProfile, c *corev1.Container, env map[string]string, t time.Time) RunPlan {
	plan := RunPlan{LongRunning: true, Ports: p.Ports}
	ts := func(off int, msg string) string {
		return t.Add(time.Duration(off)*time.Millisecond).UTC().Format(time.RFC3339) + " " + msg
	}
	if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && p.MemoryMB > 0 {
		if lim.Value() < p.MemoryMB*1024*1024 {
			plan.LongRunning = false
			plan.ExitCode = 137
			plan.Reason = "OOMKilled"
			plan.RunFor = 3 * time.Second
			if p.Logs != nil {
				plan.Logs = p.Logs(c, t)
			}
			return plan
		}
	}
	cmd := strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")
	if p.Shell {
		plan.Ports = nil
		switch {
		case cmd == "":
			// Default shell with no TTY exits immediately.
			plan.LongRunning = false
			plan.Reason = "Completed"
			plan.RunFor = time.Second
		case strings.Contains(cmd, "sleep infinity") || strings.Contains(cmd, "tail -f") || strings.Contains(cmd, "while true"):
			plan.LongRunning = true
		case strings.Contains(cmd, "sleep"):
			// "sleep 3600" etc.
			f := strings.Fields(cmd[strings.Index(cmd, "sleep")+5:])
			secs := 3600
			if len(f) > 0 {
				if n, err := strconv.Atoi(strings.Trim(f[0], ";&'\"")); err == nil {
					secs = n
				}
			}
			if secs >= 300 {
				plan.LongRunning = true
			} else {
				plan.LongRunning = false
				plan.RunFor = time.Duration(secs) * time.Second
				plan.Reason = "Completed"
			}
		default:
			plan.LongRunning = false
			plan.Reason = "Completed"
			plan.RunFor = time.Second
		}
		if m := exitRe.FindStringSubmatch(cmd); m != nil && m[1] != "0" {
			n, _ := strconv.Atoi(m[1])
			plan.LongRunning = false
			plan.ExitCode = int32(n)
			plan.Reason = "Error"
			if plan.RunFor == 0 {
				plan.RunFor = time.Second
			}
		}
		plan.Logs = echoLogs(cmd)
		return plan
	}
	for _, k := range p.RequiredEnv {
		if _, ok := env[k]; !ok {
			plan.LongRunning = false
			plan.ExitCode = 1
			plan.Reason = "Error"
			plan.RunFor = 2 * time.Second
			if p.Logs != nil {
				plan.Logs = append(plan.Logs, p.Logs(c, t)[:1]...)
			}
			plan.Logs = append(plan.Logs,
				ts(120, fmt.Sprintf("FATAL: required environment variable %s is not set", k)),
				ts(121, "exiting with status 1"))
			return plan
		}
	}
	if p.Logs != nil {
		plan.Logs = p.Logs(c, t)
	}
	return plan
}

var echoRe = regexp.MustCompile(`echo\s+(?:"([^"]*)"|'([^']*)'|([^;&|]+))`)

func echoLogs(cmd string) []string {
	var out []string
	for _, m := range echoRe.FindAllStringSubmatch(cmd, -1) {
		s := m[1] + m[2] + m[3]
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func nginxLogs(_ *corev1.Container, t time.Time) []string {
	d := t.UTC().Format("2006/01/02 15:04:05")
	return []string{
		"/docker-entrypoint.sh: /docker-entrypoint.d/ is not empty, will attempt to perform configuration",
		"/docker-entrypoint.sh: Looking for shell scripts in /docker-entrypoint.d/",
		"/docker-entrypoint.sh: Launching /docker-entrypoint.d/10-listen-on-ipv6-by-default.sh",
		"10-listen-on-ipv6-by-default.sh: info: Getting the checksum of /etc/nginx/conf.d/default.conf",
		"10-listen-on-ipv6-by-default.sh: info: Enabled listen on IPv6 in /etc/nginx/conf.d/default.conf",
		"/docker-entrypoint.sh: Configuration complete; ready for start up",
		d + " [notice] 1#1: using the \"epoll\" event method",
		d + " [notice] 1#1: nginx/1.27.3 (simulated by kubedrill)",
		d + " [notice] 1#1: start worker processes",
		d + " [notice] 1#1: start worker process 29",
	}
}

func redisLogs(_ *corev1.Container, t time.Time) []string {
	d := t.UTC().Format("02 Jan 2006 15:04:05.000")
	return []string{
		"1:C " + d + " * oO0OoO0OoO0Oo Redis is starting oO0OoO0OoO0Oo",
		"1:M " + d + " * Running mode=standalone, port=6379. (simulated by kubedrill)",
		"1:M " + d + " * Ready to accept connections tcp",
	}
}

func postgresLogs(_ *corev1.Container, t time.Time) []string {
	d := t.UTC().Format("2006-01-02 15:04:05.000 UTC")
	return []string{
		"PostgreSQL Database directory appears to contain a database; Skipping initialization",
		d + " [1] LOG:  starting PostgreSQL (simulated by kubedrill)",
		d + " [1] LOG:  listening on IPv4 address \"0.0.0.0\", port 5432",
		d + " [1] LOG:  database system is ready to accept connections",
	}
}

func demoAPILogs(_ *corev1.Container, t time.Time) []string {
	ts := func(off int, msg string) string {
		return t.Add(time.Duration(off)*time.Millisecond).UTC().Format(time.RFC3339) + " " + msg
	}
	return []string{
		ts(0, "INFO demo-api v1.4.2 starting (simulated by kubedrill)"),
		ts(40, "INFO loading configuration from environment"),
		ts(95, "INFO connected to database"),
		ts(110, "INFO listening on :8080 (health: /healthz, readiness: /ready)"),
	}
}

func genericServerLogs(name string, port int32) func(*corev1.Container, time.Time) []string {
	return func(_ *corev1.Container, t time.Time) []string {
		ts := t.UTC().Format(time.RFC3339)
		return []string{
			fmt.Sprintf("%s [INFO] %s starting (simulated by kubedrill)", ts, name),
			fmt.Sprintf("%s [INFO] listening on port %d", ts, port),
		}
	}
}
