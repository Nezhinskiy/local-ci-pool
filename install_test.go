package localcipool

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The installer is driven with stubs for every tool that would touch the real
// machine: gh, docker, launchctl, curl, scutil and uname. HOME is a temporary
// directory, so nothing is written under the real ~/Library, and no test
// reaches the real launchctl.

// stubUID is what the stub id prints: not 0, whoever runs the tests.
const stubUID = "501"

const (
	label        = "com.local-ci-pool.pool"
	templateName = label + ".plist.tmpl"
)

// stubScripts are the stub commands. Each appends what it was called with to
// $STUB_LOG; the ones whose behaviour a test varies read STUB_* variables.
var stubScripts = map[string]string{
	"gh": `#!/bin/sh
# The flag probe is logged apart, so "gh attestation verify " is the check itself.
if [ "$1 $2 $3" = "attestation verify --help" ]; then
	echo "gh-help attestation verify" >> "$STUB_LOG"
	echo "      --signer-workflow string   Enforce that the workflow that signed the attestation matches"
	[ "${STUB_GH_OLD:-0}" = 1 ] || echo "      --deny-self-hosted-runners   Fail verification for attestations generated on self-hosted runners"
	exit 0
fi
echo "gh $*" >> "$STUB_LOG"
case "$1 $2" in
"auth status") exit "${STUB_GH_AUTH_EXIT:-0}" ;;
"release view") echo "${STUB_LATEST:-v0.1.0}"; exit 0 ;;
"release download")
	dir=""; pattern=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--dir) dir="$2"; shift ;;
		--pattern) pattern="$2"; shift ;;
		esac
		shift
	done
	mkdir -p "$dir"
	arch=arm64
	case "$pattern" in *amd64*) arch=amd64 ;; esac
	tar -czf "$dir/local-ci-pool_0.1.0_darwin_$arch.tar.gz" -C "$STUB_ARCHIVE_SRC" .
	exit 0 ;;
"attestation verify")
	if [ -e "$HOME/Library/Application Support/local-ci-pool" ] || [ -e "$HOME/Library/LaunchAgents/` + label + `.plist" ]; then
		echo "PLACED-BEFORE-VERIFY" >> "$STUB_LOG"
	fi
	exit "${STUB_VERIFY_EXIT:-0}" ;;
esac
exit 0
`,
	"docker": `#!/bin/sh
echo "docker $*" >> "$STUB_LOG"
[ "${STUB_DOCKER_EXIT:-0}" = 0 ] || exit "$STUB_DOCKER_EXIT"
echo "${STUB_DOCKER_INFO:-19327352832 10}"
`,
	"launchctl": `#!/bin/sh
echo "launchctl $*" >> "$STUB_LOG"
b=gone; p=gone
[ -e "$HOME/Library/Application Support/local-ci-pool/bin/pool" ] && b=present
[ -e "$HOME/Library/LaunchAgents/` + label + `.plist" ] && p=present
case "$1" in
bootout)
	echo "at-bootout binary=$b plist=$p" >> "$STUB_LOG"
	rm -f "$STUB_LOG.loaded" "$STUB_LOG.kicked"
	touch "$STUB_LOG.out"
	[ -z "$STUB_BOOTOUT_MSG" ] || echo "$STUB_BOOTOUT_MSG" >&2
	exit "${STUB_BOOTOUT_EXIT:-0}" ;;
print)
	echo "at-print binary=$b" >> "$STUB_LOG"
	touch "$STUB_LOG.printed"
	if [ ! -e "$STUB_LOG.out" ]; then
		# Before a bootout the agent is loaded when STUB_POOL_PID names a
		# process (or STUB_RUNNING_NO_PID is set), and launchd shows the pid
		# while that process runs, the way launchctl print lays it out.
		if [ -n "${STUB_RUNNING_NO_PID:-}" ]; then
			printf 'gui/501/x = {\n\tstate = running\n}\n'
			exit 0
		fi
		[ -n "${STUB_POOL_PID:-}" ] || exit 113
		# A nested block with its own "pid =" line comes before the
		# service's, which is indented by one tab.
		# After STUB_PID_PRINTS such calls launchd reports STUB_NEXT_PID
		# instead (a restart, or the pid now names another process).
		pid="$STUB_POOL_PID"
		if [ -n "${STUB_PID_PRINTS:-}" ]; then
			n=$(cat "$STUB_LOG.pidprints" 2>/dev/null || echo 0)
			echo $((n + 1)) > "$STUB_LOG.pidprints"
			[ "$n" -lt "$STUB_PID_PRINTS" ] || pid="${STUB_NEXT_PID:-}"
		fi
		printf 'gui/501/` + label + ` = {\n\tactive count = 1\n\tpath = /x.plist\n'
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			printf '\tstate = running\n\tprogram = /x/pool\n\tenvironment = {\n\t\tXPC_SERVICE_NAME => x\n\t}\n\tendpoints = {\n\t\t"x" = {\n\t\t\tpid = 99999999\n\t\t}\n\t}\n\truns = 1\n\tpid = %s\n\timmediate reason = speculative\n' "$pid"
		else
			printf '\tstate = not running\n\truns = 1\n\tlast exit code = 0\n'
		fi
		printf '}\n'
		exit 0
	fi
	# After a bootout the agent stays visible for STUB_PRINT_SUCCESSES calls,
	# then is gone.
	f="$STUB_LOG.print"
	n=$(cat "$f" 2>/dev/null || echo "${STUB_PRINT_SUCCESSES:-0}")
	if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$f"; exit 0; fi
	echo 0 > "$f"
	exit 113 ;;
bootstrap)
	echo "at-bootstrap binary=$b plist=$p" >> "$STUB_LOG"
	[ "${STUB_BOOTSTRAP_EXIT:-0}" = 0 ] || exit "$STUB_BOOTSTRAP_EXIT"
	touch "$STUB_LOG.loaded"
	rm -f "$STUB_LOG.out"
	if [ -n "${STUB_LOG_AT_LOAD:-}" ]; then
		mkdir -p "$HOME/Library/Logs/local-ci-pool"
		echo "$STUB_LOG_AT_LOAD" >> "$HOME/Library/Logs/local-ci-pool/pool.log"
	fi
	exit 0 ;;
kickstart)
	touch "$STUB_LOG.kicked"
	exit 0 ;;
esac
exit 0
`,
	// Before the agent is loaded, curl reaches the old pool for
	// STUB_PORT_BUSY calls, then nothing listens (exit 7). Once it is loaded,
	// /healthz reports STUB_NEW_VERSION, and STUB_KICKED_VERSION after a
	// kickstart; "none" means nothing listens.
	"curl": `#!/bin/sh
echo "curl $*" >> "$STUB_LOG"
if [ -e "$STUB_LOG.loaded" ]; then
	v="${STUB_NEW_VERSION:-v0.1.0}"
	if [ -e "$STUB_LOG.kicked" ] && [ -n "${STUB_KICKED_VERSION:-}" ]; then v="$STUB_KICKED_VERSION"; fi
	[ "$v" != none ] || exit 7
	echo "{\"version\":\"$v\",\"commit\":\"abc1234\",\"machine\":\"examplemacbo\",\"slots\":4,\"projects\":[]}"
	exit 0
fi
f="$STUB_LOG.busy"
n=$(cat "$f" 2>/dev/null || echo "${STUB_PORT_BUSY:-0}")
if [ "$n" -gt 0 ]; then
	echo $((n - 1)) > "$f"
	echo '{"version":"v0.0.9"}'
	exit 0
fi
echo 0 > "$f"
exit 7
`,
	"scutil": `#!/bin/sh
echo "scutil $*" >> "$STUB_LOG"
echo "${STUB_HOST:-Example-MacBook}"
`,
	"mv": `#!/bin/sh
echo "mv $*" >> "$STUB_LOG"
[ "${STUB_MV_FAIL:-0}" = 0 ] || exit 1
exec /bin/mv "$@"
`,
	"plutil": `#!/bin/sh
echo "plutil $*" >> "$STUB_LOG"
exit "${STUB_PLUTIL_EXIT:-0}"
`,
	"sysctl": `#!/bin/sh
echo "sysctl $*" >> "$STUB_LOG"
echo "${STUB_ARM64:-0}"
`,
	"id": `#!/bin/sh
case "$1" in
-u) echo "${STUB_UID:-501}" ;;
*) /usr/bin/id "$@" ;;
esac
`,
	"uname": `#!/bin/sh
case "$1" in
-s) echo "${STUB_OS:-Darwin}" ;;
-m) echo "${STUB_ARCH:-arm64}" ;;
*) echo "${STUB_OS:-Darwin}" ;;
esac
`,
}

// stubPool is the pool binary inside the stub archive.
const stubPool = `#!/bin/sh
echo "pool $*" >> "$STUB_LOG"
case "$1" in
version) echo "v0.1.0 abc1234" ;;
print-defaults) echo "{\"drain_seconds\":2400,\"stop_seconds\":2580,\"health_addr\":\"${STUB_HEALTH_ADDR:-127.0.0.1:8737}\"}" ;;
forget)
	p=gone
	[ -e "$HOME/Library/LaunchAgents/` + label + `.plist" ] && p=present
	echo "at-forget plist=$p" >> "$STUB_LOG"
	exit "${STUB_FORGET_EXIT:-0}" ;;
esac
`

type env struct {
	t       *testing.T
	home    string
	stubs   string
	archive string // the tree the stub gh packs into the release archive
	log     string
	vars    map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	root := t.TempDir()
	e := &env{
		t:       t,
		home:    filepath.Join(root, "home"),
		stubs:   filepath.Join(root, "stubs"),
		archive: filepath.Join(root, "archive"),
		log:     filepath.Join(root, "calls.log"),
		vars:    map[string]string{},
	}
	for _, d := range []string{e.home, e.stubs, filepath.Join(e.archive, "launchd")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range stubScripts {
		write(t, filepath.Join(e.stubs, name), body, 0o755)
	}
	write(t, filepath.Join(e.archive, "pool"), stubPool, 0o755)
	tmpl, err := os.ReadFile(filepath.Join("launchd", templateName))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.archive, "launchd", templateName), string(tmpl), 0o644)
	write(t, e.log, "", 0o644)
	return e
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func (e *env) binPath() string {
	return filepath.Join(e.home, "Library", "Application Support", "local-ci-pool", "bin", "pool")
}

func (e *env) plistPath() string {
	return filepath.Join(e.home, "Library", "LaunchAgents", label+".plist")
}

// install runs install.sh with the stubs first on PATH. It returns the combined
// output and the exit error, if any.
func (e *env) install(args ...string) (string, error) {
	e.t.Helper()
	bash, _ := exec.LookPath("bash")
	cmd := exec.Command(bash, append([]string{"install.sh"}, args...)...)
	cmd.Env = []string{
		"HOME=" + e.home,
		"PATH=" + e.stubs + ":/usr/bin:/bin",
		"STUB_LOG=" + e.log,
		"STUB_ARCHIVE_SRC=" + e.archive,
		"LOCAL_CI_INSTALL_POLL=0.05",
		"LOCAL_CI_INSTALL_HEALTH_POLL=0.05",
		"TMPDIR=" + e.t.TempDir(),
	}
	for k, v := range e.vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (e *env) calls() []string {
	b, err := os.ReadFile(e.log)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// index returns the position of the first call that starts with prefix, or -1.
func index(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestInstallDryRunVerifiesAttestation(t *testing.T) {
	e := newEnv(t)
	out, err := e.install("--dry-run")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	calls := e.calls()
	if index(calls, "gh attestation verify ") < 0 {
		t.Fatalf("the attestation was never verified; calls: %v", calls)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "--repo Nezhinskiy/local-ci-pool") {
		t.Errorf("verify is not pinned to this repository: %v", calls)
	}
	verify := calls[index(calls, "gh attestation verify ")]
	for _, want := range []string{
		"--signer-workflow Nezhinskiy/local-ci-pool/.github/workflows/release.yml",
		"--source-ref refs/tags/v0.1.0",
	} {
		if !strings.Contains(verify, want) {
			t.Errorf("the provenance check lacks %q: %s", want, verify)
		}
	}
	if index(calls, "gh release download ") > index(calls, "gh attestation verify ") {
		t.Errorf("the release was verified before it was downloaded: %v", calls)
	}
	for _, c := range calls {
		if c == "PLACED-BEFORE-VERIFY" {
			t.Fatal("something was placed before the attestation was verified")
		}
		if strings.HasPrefix(c, "launchctl ") {
			t.Errorf("a dry run called launchctl: %q", c)
		}
	}
	if exists(filepath.Join(e.home, "Library")) {
		t.Errorf("a dry run placed files under %s/Library", e.home)
	}
	for _, want := range []string{
		"would: stop a running pool, if any: kill -TERM", "would: unload the agent: launchctl bootout",
		"would: wait until launchd no longer lists it",
		"would: stage the binary", "would: render the launchd agent", "would: replace", "would: load the agent",
		"would: wait until nothing answers on 127.0.0.1:8737",
		"would: wait for http://127.0.0.1:8737/healthz to report v0.1.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not announce %q:\n%s", want, out)
		}
	}
}

func TestInstallRefusesWhenAttestationFailsAndPlacesNothing(t *testing.T) {
	for name, args := range map[string][]string{"install": nil, "dry run": {"--dry-run"}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.vars["STUB_VERIFY_EXIT"] = "1"
			out, err := e.install(args...)
			if err == nil {
				t.Fatalf("the install succeeded although the attestation failed:\n%s", out)
			}
			if !strings.Contains(out, "did not verify") {
				t.Errorf("output does not say the verification failed:\n%s", out)
			}
			if exists(filepath.Join(e.home, "Library")) {
				t.Error("files were placed although the attestation failed")
			}
			for _, c := range e.calls() {
				if strings.HasPrefix(c, "launchctl ") || strings.HasPrefix(c, "pool ") {
					t.Errorf("%q ran after a failed verification", c)
				}
			}
		})
	}
}

func TestInstallVerifiesBeforePlacingAndLoadsTheAgent(t *testing.T) {
	e := newEnv(t)
	out, err := e.install("--version", "v0.1.0")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	calls := e.calls()
	for _, c := range calls {
		if c == "PLACED-BEFORE-VERIFY" {
			t.Fatal("something was placed before the attestation was verified")
		}
	}
	if !strings.Contains(strings.Join(calls, "\n"), "gh release download v0.1.0 --repo Nezhinskiy/local-ci-pool") {
		t.Errorf("the requested version was not downloaded: %v", calls)
	}
	uid := stubUID
	bootout := index(calls, "launchctl bootout gui/"+uid+"/"+label)
	bootstrap := index(calls, "launchctl bootstrap gui/"+uid+" "+e.plistPath())
	if bootout < 0 || bootstrap < bootout {
		t.Errorf("want bootout, then bootstrap; calls: %v", calls)
	}
	if !exists(e.binPath()) || !exists(e.plistPath()) {
		t.Errorf("binary %v, plist %v", exists(e.binPath()), exists(e.plistPath()))
	}
	if fi, err := os.Stat(e.binPath()); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("the binary is not executable: %v %v", fi, err)
	}
	if !exists(filepath.Join(e.home, "Library", "Logs", "local-ci-pool")) {
		t.Error("the log directory was not created")
	}
	if !strings.Contains(out, "4 slots") || !strings.Contains(out, "CI_POOL_HB_EXAMPLEMACBO") {
		t.Errorf("output lacks the slots and the heartbeat variable:\n%s", out)
	}
}

func TestInstallIdempotentNoConfig(t *testing.T) {
	e := newEnv(t)
	first, err := e.install("--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, first)
	}
	second, err := e.install("--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, second)
	}
	if first != second {
		t.Fatalf("two dry runs differ:\n--- first\n%s--- second\n%s", first, second)
	}

	// Two real installs leave the same files and print the same actions.
	a, err := e.install()
	if err != nil {
		t.Fatalf("%v\n%s", err, a)
	}
	plist1, _ := os.ReadFile(e.plistPath())
	bin1, _ := os.ReadFile(e.binPath())
	b, err := e.install()
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	plist2, _ := os.ReadFile(e.plistPath())
	bin2, _ := os.ReadFile(e.binPath())
	if a != b || !bytes.Equal(plist1, plist2) || !bytes.Equal(bin1, bin2) {
		t.Errorf("a second install differs from the first:\n--- first\n%s--- second\n%s", a, b)
	}

	// Configuration is never written, in any mode.
	for _, p := range []string{".config", ".docker", ".gitconfig", ".zshrc", ".bash_profile", ".ssh"} {
		if exists(filepath.Join(e.home, p)) {
			t.Errorf("the installer wrote %s", p)
		}
	}
	entries, err := os.ReadDir(filepath.Join(e.home, "Library"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, en := range entries {
		got = append(got, en.Name())
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "Application Support LaunchAgents Logs" {
		t.Errorf("Library holds %v, want only Application Support, LaunchAgents and Logs", got)
	}
}

func TestInstallPicksTheArchiveByArchitecture(t *testing.T) {
	for _, tc := range []struct{ uname, pattern string }{
		{"arm64", "--pattern *darwin_arm64*"},
		{"x86_64", "--pattern *darwin_amd64*"},
	} {
		t.Run(tc.uname, func(t *testing.T) {
			e := newEnv(t)
			e.vars["STUB_ARCH"] = tc.uname
			if out, err := e.install("--dry-run"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if i := index(e.calls(), "gh release download "); i < 0 || !strings.Contains(e.calls()[i], tc.pattern) {
				t.Fatalf("calls %v, want %q", e.calls(), tc.pattern)
			}
		})
	}
	t.Run("unsupported", func(t *testing.T) {
		e := newEnv(t)
		e.vars["STUB_ARCH"] = "riscv64"
		out, err := e.install("--dry-run")
		if err == nil || !strings.Contains(out, "unsupported architecture") {
			t.Fatalf("want a refusal, got %v\n%s", err, out)
		}
		if index(e.calls(), "gh release download ") >= 0 {
			t.Error("a release was downloaded for an unsupported architecture")
		}
	})
}

func TestInstallRefusesBeforeDownloading(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		want string
	}{
		"gh logged out":      {map[string]string{"STUB_GH_AUTH_EXIT": "1"}, "gh auth login"},
		"docker not running": {map[string]string{"STUB_DOCKER_EXIT": "1"}, "Docker is not answering"},
		// 5 GiB: (5 - 2) / 4 = 0 slots.
		"too little memory": {map[string]string{"STUB_DOCKER_INFO": "5368709120 10"}, "too little for one runner slot"},
		// 18 GiB but 1 CPU: 0 slots.
		"too few CPUs": {map[string]string{"STUB_DOCKER_INFO": "19327352832 1"}, "too little for one runner slot"},
		"not macOS":    {map[string]string{"STUB_OS": "Linux"}, "macOS only"},
		"bad host":     {map[string]string{"STUB_HOST": "---"}, "no letters or digits"},
		"garbled info": {map[string]string{"STUB_DOCKER_INFO": "lots of memory"}, "unexpected output of docker info"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			for k, v := range tc.vars {
				e.vars[k] = v
			}
			out, err := e.install()
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("want a refusal mentioning %q, got %v\n%s", tc.want, err, out)
			}
			if index(e.calls(), "gh release download ") >= 0 || exists(filepath.Join(e.home, "Library")) {
				t.Errorf("the installer went on after refusing; calls %v", e.calls())
			}
		})
	}
}

// The installer prints the same slot count the pool computes (memory rounded
// to the nearest GiB, then the CPU and the cap).
func TestInstallSlotsMatchThePool(t *testing.T) {
	const gib = 1 << 30
	for _, tc := range []struct {
		mem   int64
		cpus  int
		slots int
	}{
		{10 * gib, 10, 2},
		{8319504384, 10, 1},  // 7.75 GiB reads as 8 GiB
		{10413000000, 10, 2}, // a 10 GB slider reads 9.7 GiB: rounded to 10, not floored to 9
		{18 * gib, 10, 4},
		{18 * gib, 6, 3},
		{100 * gib, 64, 8},
		{6 * gib, 2, 1},
	} {
		e := newEnv(t)
		e.vars["STUB_DOCKER_INFO"] = fmt.Sprintf("%d %d", tc.mem, tc.cpus)
		out, err := e.install("--dry-run")
		if err != nil {
			t.Fatalf("%+v: %v\n%s", tc, err, out)
		}
		if want := fmt.Sprintf("(%d slots;", tc.slots); !strings.Contains(out, want) {
			t.Errorf("mem %d, cpus %d: output lacks %q:\n%s", tc.mem, tc.cpus, want, out)
		}
	}
}

func TestInstallRejectsABadVersion(t *testing.T) {
	e := newEnv(t)
	out, err := e.install("--version", "latest; rm -rf /")
	if err == nil || !strings.Contains(out, "--version must look like") {
		t.Fatalf("want a refusal, got %v\n%s", err, out)
	}
	if len(e.calls()) != 0 {
		t.Errorf("tools ran: %v", e.calls())
	}
}

// placeInstalled puts a stub pool and a plist where a previous install left them.
func (e *env) placeInstalled() {
	write(e.t, e.binPath(), stubPool, 0o755)
	write(e.t, e.plistPath(), "<plist/>\n", 0o644)
}

func TestUninstallBootsOutWaitsThenForgets(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.vars["STUB_PRINT_SUCCESSES"] = "2"
	out, err := e.install("--uninstall")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	calls := e.calls()
	bootout := index(calls, "launchctl bootout gui/"+stubUID+"/"+label)
	forget := index(calls, "pool forget")
	if bootout < 0 || forget < 0 || bootout > forget {
		t.Fatalf("want bootout, then pool forget; calls: %v", calls)
	}
	// The heartbeat dies with the process, so the variable is deleted only
	// after launchd has let go of the pool: every print is before the forget,
	// and there are 2 that still see the agent plus the one that does not.
	prints := 0
	for _, c := range calls[bootout:forget] {
		if strings.HasPrefix(c, "launchctl print gui/"+stubUID+"/"+label) {
			prints++
		}
	}
	if prints != 3 {
		t.Errorf("%d launchctl print calls between bootout and forget, want 3: %v", prints, calls)
	}
	// Nothing is removed before the forget has run.
	if at := index(calls, "at-forget "); at < 0 || calls[at] != "at-forget plist=present" {
		t.Errorf("the plist was removed before the forget: %v", calls)
	}
	if exists(e.binPath()) || exists(e.plistPath()) {
		t.Errorf("binary %v, plist %v still exist", exists(e.binPath()), exists(e.plistPath()))
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "gh ") {
			t.Errorf("uninstall called gh: %q", c)
		}
	}
}

// A pool that never leaves is reported; nothing is forgotten or removed under it.
func TestUninstallGivesUpWhenThePoolNeverLeaves(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.vars["STUB_PRINT_SUCCESSES"] = "1000000"
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "1"
	out, err := e.install("--uninstall")
	if err == nil || !strings.Contains(out, "launchd still has") {
		t.Fatalf("want a refusal, got %v\n%s", err, out)
	}
	if index(e.calls(), "pool forget") >= 0 || !exists(e.binPath()) || !exists(e.plistPath()) {
		t.Errorf("the uninstall went on although the pool is still there: %v", e.calls())
	}
}

func TestUninstallGoesOnWhenForgetFails(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.vars["STUB_FORGET_EXIT"] = "1"
	out, err := e.install("--uninstall")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "could not delete the heartbeat variables") {
		t.Errorf("the failed forget was not reported:\n%s", out)
	}
	if index(e.calls(), "launchctl bootout") < 0 || exists(e.binPath()) || exists(e.plistPath()) {
		t.Errorf("the uninstall stopped at the failed forget; calls %v", e.calls())
	}
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	out, err := e.install("--uninstall", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(e.calls()) != 0 {
		t.Errorf("a dry run ran %v", e.calls())
	}
	if !exists(e.binPath()) || !exists(e.plistPath()) {
		t.Error("a dry run removed files")
	}
	for _, want := range []string{"would: stop the pool, if it runs: kill -TERM", "would: unload the agent", "would: wait until launchd no longer lists it", "would: delete this Mac's heartbeat variables", "would: remove"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// plistValue is a decoded property list value: a map, a slice, a string, an
// int or a bool.
type plistValue = any

// parsePlist decodes the XML property list subset the template uses.
func parsePlist(t *testing.T, b []byte) plistValue {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	// Find <plist>, then decode its single child.
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("no <plist> element: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "plist" {
			break
		}
	}
	v, err := plistNext(dec)
	if err != nil {
		t.Fatalf("parsing the plist: %v", err)
	}
	return v
}

func nextStart(dec *xml.Decoder) (xml.StartElement, bool, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, false, err
		}
		switch tt := tok.(type) {
		case xml.StartElement:
			return tt, true, nil
		case xml.EndElement:
			return xml.StartElement{}, false, nil
		}
	}
}

func plistNext(dec *xml.Decoder) (plistValue, error) {
	se, ok, err := nextStart(dec)
	if err != nil || !ok {
		return nil, err
	}
	return plistDecode(dec, se)
}

func plistDecode(dec *xml.Decoder, se xml.StartElement) (plistValue, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]plistValue{}
		for {
			k, ok, err := nextStart(dec)
			if err != nil {
				return nil, err
			}
			if !ok {
				return m, nil
			}
			if k.Name.Local != "key" {
				return nil, fmt.Errorf("want <key>, got <%s>", k.Name.Local)
			}
			var name string
			if err := dec.DecodeElement(&name, &k); err != nil {
				return nil, err
			}
			v, err := plistNext(dec)
			if err != nil {
				return nil, err
			}
			m[name] = v
		}
	case "array":
		var a []plistValue
		for {
			e, ok, err := nextStart(dec)
			if err != nil {
				return nil, err
			}
			if !ok {
				return a, nil
			}
			v, err := plistDecode(dec, e)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
	case "string":
		var s string
		return s, dec.DecodeElement(&s, &se)
	case "integer":
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		return strconv.Atoi(strings.TrimSpace(s))
	case "true", "false":
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	return nil, fmt.Errorf("unsupported plist element <%s>", se.Name.Local)
}

func dict(t *testing.T, v plistValue, what string) map[string]plistValue {
	t.Helper()
	m, ok := v.(map[string]plistValue)
	if !ok {
		t.Fatalf("%s is %T, want a dict", what, v)
	}
	return m
}

func TestPlistKeys(t *testing.T) {
	e := newEnv(t)
	if out, err := e.install(); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(e.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "@") {
		t.Errorf("a placeholder is left in the plist:\n%s", raw)
	}
	top := dict(t, parsePlist(t, raw), "the plist")

	if top["Label"] != label {
		t.Errorf("Label = %v", top["Label"])
	}
	keep := dict(t, top["KeepAlive"], "KeepAlive")
	if keep["SuccessfulExit"] != false {
		t.Errorf("KeepAlive.SuccessfulExit = %v, want false (a clean exit must not restart the pool)", keep["SuccessfulExit"])
	}
	if top["ThrottleInterval"] != 30 {
		t.Errorf("ThrottleInterval = %v, want 30", top["ThrottleInterval"])
	}
	if top["ProcessType"] != "Background" {
		t.Errorf("ProcessType = %v, want Background", top["ProcessType"])
	}
	if top["RunAtLoad"] != true {
		t.Errorf("RunAtLoad = %v, want true", top["RunAtLoad"])
	}
	args, _ := top["ProgramArguments"].([]plistValue)
	if len(args) != 2 || args[0] != e.binPath() || args[1] != "run" {
		t.Errorf("ProgramArguments = %v, want [%s run]", args, e.binPath())
	}
	logs := filepath.Join(e.home, "Library", "Logs", "local-ci-pool", "pool.log")
	if top["StandardOutPath"] != logs || top["StandardErrorPath"] != logs {
		t.Errorf("log paths = %v, %v, want %s", top["StandardOutPath"], top["StandardErrorPath"], logs)
	}
	path, _ := dict(t, top["EnvironmentVariables"], "EnvironmentVariables")["PATH"].(string)
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"} {
		if !hasPathEntry(path, d) {
			t.Errorf("PATH %q lacks %s", path, d)
		}
	}
	// The pool shells out to scutil and ioreg, which live in /usr/sbin.
	if !hasPathEntry(path, "/usr/sbin") {
		t.Errorf("PATH %q lacks /usr/sbin (scutil, ioreg)", path)
	}

	// ExitTimeOut stays, but it does not protect the drain: macOS clamps a
	// LaunchAgent's exit timeout to 60 s (launchctl print reports
	// "exit timeout = 60" for this plist on macOS 27.0.1), so a bootout or a
	// logout SIGKILLs a pool still draining after a minute. What protects the
	// drain is that the installer, not launchd, sends the stop signal and
	// waits for the process (TestUpgradeSignalsThePoolItselfBeforeTheBootout,
	// TestUninstallSignalsThePoolItselfBeforeTheBootout), and that its wait
	// outlasts the drain the pool reports. The supervisor's own test keeps the
	// drain plus the cleanup after it under 45 minutes.
	if top["ExitTimeOut"] != 2700 {
		t.Errorf("ExitTimeOut = %v, want 2700", top["ExitTimeOut"])
	}
	stop := poolDefaults(t).StopSeconds
	if wait := installerWaitLimit(t); wait <= stop || wait < 45*60 {
		t.Errorf("the installer waits %d s for the pool to exit; it must exceed the pool's longest stop %d s and 45 minutes", wait, stop)
	}
}

// installerWaitLimit reads the default of WAIT_LIMIT from install.sh.
func installerWaitLimit(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^WAIT_LIMIT="\$\{LOCAL_CI_INSTALL_WAIT_LIMIT:-([0-9]+)\}"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("install.sh has no WAIT_LIMIT default")
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

func hasPathEntry(path, dir string) bool {
	for _, p := range strings.Split(path, ":") {
		if p == dir {
			return true
		}
	}
	return false
}

type defaultsJSON struct {
	DrainSeconds int    `json:"drain_seconds"`
	StopSeconds  int    `json:"stop_seconds"`
	HealthAddr   string `json:"health_addr"`
}

// poolDefaults reads the defaults from the real program, so the plist is
// checked against the number the pool uses and not a copy of it.
func poolDefaults(t *testing.T) defaultsJSON {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	cmd := exec.Command(goBin, "run", "./cmd/pool", "print-defaults")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go run ./cmd/pool print-defaults: %v\n%s", err, stderr.String())
	}
	var d defaultsJSON
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("print-defaults printed %q: %v", out, err)
	}
	if d.DrainSeconds <= 0 {
		t.Fatalf("print-defaults = %+v", d)
	}
	return d
}

// A home directory with characters that matter to sed and XML still gives a
// valid plist with the right paths.
func TestPlistRenderingEscapesPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	e := newEnv(t)
	home := filepath.Join(filepath.Dir(e.home), "ho&me|dir")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	e.home = home
	if out, err := e.install(); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(e.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	top := dict(t, parsePlist(t, raw), "the plist")
	args, _ := top["ProgramArguments"].([]plistValue)
	if len(args) != 2 || args[0] != e.binPath() {
		t.Fatalf("ProgramArguments = %v, want %s first", args, e.binPath())
	}
}

func TestInstallWaitsForTheDrainedAgentBeforeReplacingAndBootstrapping(t *testing.T) {
	e := newEnv(t)
	e.vars["STUB_PRINT_SUCCESSES"] = "3"
	out, err := e.install()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	calls := e.calls()
	bootout := index(calls, "launchctl bootout ")
	bootstrap := index(calls, "launchctl bootstrap ")
	prints := 0
	for _, c := range calls[bootout:bootstrap] {
		if strings.HasPrefix(c, "launchctl print gui/"+stubUID+"/"+label) {
			prints++
		}
		// The new binary is not put under a pool that is still draining.
		if strings.HasPrefix(c, "at-print ") && c != "at-print binary=gone" {
			t.Errorf("the binary was replaced while launchd still listed the pool: %v", calls)
		}
	}
	if bootout < 0 || bootstrap < 0 || prints != 4 {
		t.Fatalf("want bootout, 3 sightings plus 1 absence, then bootstrap (got %d prints); calls: %v", prints, calls)
	}
	if at := index(calls, "at-bootstrap "); calls[at] != "at-bootstrap binary=present plist=present" {
		t.Errorf("bootstrap ran before the agent was placed: %v", calls)
	}
}

func TestInstallGivesUpWhenLaunchdKeepsThePool(t *testing.T) {
	e := newEnv(t)
	e.vars["STUB_PRINT_SUCCESSES"] = "1000000"
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "3"
	e.vars["LOCAL_CI_INSTALL_PROGRESS"] = "1"
	out, err := e.install()
	if err == nil || !strings.Contains(out, "launchd still has") {
		t.Fatalf("want a refusal, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "still waiting for launchd to unload the agent") {
		t.Errorf("no progress line while waiting:\n%s", out)
	}
	if index(e.calls(), "launchctl bootstrap") >= 0 || exists(e.binPath()) || exists(e.plistPath()) {
		t.Errorf("the install went on although the old pool is still there: %v", e.calls())
	}
	if exists(e.binPath()+".new") || exists(e.plistPath()+".new") {
		t.Error("a staged file was left behind")
	}
}

func TestInstallShowsBootoutErrorsExceptNotLoaded(t *testing.T) {
	for name, tc := range map[string]struct {
		msg  string
		exit string
		show bool
	}{
		"in progress": {"Boot-out failed: 36: Operation now in progress", "36", true},
		"io error":    {"Boot-out failed: 5: Input/output error", "5", true},
		"not loaded":  {"Boot-out failed: 3: No such process", "3", false},
		"not found":   {"Could not find service in domain", "113", false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.vars["STUB_BOOTOUT_MSG"] = tc.msg
			e.vars["STUB_BOOTOUT_EXIT"] = tc.exit
			out, err := e.install()
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if got := strings.Contains(out, tc.msg); got != tc.show {
				t.Errorf("message shown = %v, want %v:\n%s", got, tc.show, out)
			}
			if index(e.calls(), "launchctl bootstrap") < 0 {
				t.Error("the agent was not loaded")
			}
		})
	}
}

func TestInstallResolvesTheLatestTagForTheProvenanceCheck(t *testing.T) {
	e := newEnv(t)
	e.vars["STUB_LATEST"] = "v0.2.3"
	if out, err := e.install("--dry-run"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	calls := strings.Join(e.calls(), "\n")
	for _, want := range []string{"gh release download v0.2.3 --repo", "--source-ref refs/tags/v0.2.3"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}

	e = newEnv(t)
	e.vars["STUB_LATEST"] = "nightly; rm -rf /"
	out, err := e.install("--dry-run")
	if err == nil || !strings.Contains(out, "not a version tag") || index(e.calls(), "gh release download") >= 0 {
		t.Fatalf("want a refusal before downloading, got %v\n%s", err, out)
	}
}

func TestInstallLintsThePlistBeforePlacingIt(t *testing.T) {
	e := newEnv(t)
	if out, err := e.install(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	calls := e.calls()
	lint := index(calls, "plutil -lint ")
	if lint < 0 || calls[lint] != "plutil -lint "+e.plistPath()+".new" {
		t.Fatalf("want a lint of the staged file, calls: %v", calls)
	}
	if lint > index(calls, "launchctl bootout") {
		t.Errorf("the plist was linted after the running pool was unloaded: %v", calls)
	}

	// A plist that does not lint leaves the running pool and the old files alone.
	e = newEnv(t)
	e.placeInstalled() // a good agent from before
	oldPlist, _ := os.ReadFile(e.plistPath())
	oldBin, _ := os.ReadFile(e.binPath())
	e.vars["STUB_PLUTIL_EXIT"] = "1"
	out, err := e.install()
	if err == nil || !strings.Contains(out, "not a valid property list") {
		t.Fatalf("want a refusal, got %v\n%s", err, out)
	}
	for _, c := range e.calls() {
		if strings.HasPrefix(c, "launchctl ") {
			t.Errorf("the running pool was touched before the plist was known to be good: %q", c)
		}
	}
	newPlist, _ := os.ReadFile(e.plistPath())
	newBin, _ := os.ReadFile(e.binPath())
	if !bytes.Equal(oldPlist, newPlist) || !bytes.Equal(oldBin, newBin) {
		t.Error("an old file was replaced")
	}
	if exists(e.plistPath()+".new") || exists(e.binPath()+".new") {
		t.Error("a staged file was left behind")
	}
	if strings.Contains(out, "the old pool is stopped") {
		t.Errorf("the output claims the pool was stopped:\n%s", out)
	}
}

const strandedLine = "the old pool is stopped and the new one is not running; rerun install.sh (CI falls back to hosted runners meanwhile)"

// After the unload, a failing rename or load says so, and the old pool is gone.
func TestInstallSaysWhenThePoolIsDown(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"rename fails": {"STUB_MV_FAIL": "1"},
		"load fails":   {"STUB_BOOTSTRAP_EXIT": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.placeInstalled()
			for k, v := range vars {
				e.vars[k] = v
			}
			out, err := e.install()
			if err == nil {
				t.Fatalf("the install succeeded:\n%s", out)
			}
			if !strings.Contains(out, strandedLine) {
				t.Errorf("output lacks the line about the stopped pool:\n%s", out)
			}
			if index(e.calls(), "launchctl bootout") < 0 {
				t.Error("the test never reached the unload")
			}
			if exists(e.binPath()+".new") || exists(e.plistPath()+".new") {
				t.Error("a staged file was left behind")
			}
		})
	}
}

// The line is only for the case it describes.
func TestInstallStoppedLineOnlyWhenTrue(t *testing.T) {
	// success
	e := newEnv(t)
	out, err := e.install()
	if err != nil || strings.Contains(out, "is stopped") {
		t.Fatalf("a good install: %v\n%s", err, out)
	}
	// failure before the unload: attestation
	e = newEnv(t)
	e.vars["STUB_VERIFY_EXIT"] = "1"
	if out, err := e.install(); err == nil || strings.Contains(out, "is stopped") {
		t.Fatalf("verify failure: %v\n%s", err, out)
	}
	// the pool never finishes its drain: it is still running, so it is not
	// "stopped"
	e = newEnv(t)
	e.startPool(-1)
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "1"
	out, err = e.install()
	if err == nil || strings.Contains(out, "is stopped") || !strings.Contains(out, "is still finishing its jobs") {
		t.Fatalf("timeout: %v\n%s", err, out)
	}
	// launchd keeps the agent after the bootout: the message says so itself
	e = newEnv(t)
	e.vars["STUB_PRINT_SUCCESSES"] = "1000000"
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "1"
	out, err = e.install()
	if err == nil || strings.Contains(out, "is stopped") || !strings.Contains(out, "launchd still has") {
		t.Fatalf("launchd timeout: %v\n%s", err, out)
	}
	// uninstall has no new pool to miss
	e = newEnv(t)
	e.placeInstalled()
	e.startPool(-1)
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "1"
	if out, err := e.install("--uninstall"); err == nil || strings.Contains(out, "is stopped") {
		t.Fatalf("uninstall timeout: %v\n%s", err, out)
	}
}

func TestInstallDryRunReadsTheHealthAddressFromThePool(t *testing.T) {
	e := newEnv(t)
	e.vars["STUB_HEALTH_ADDR"] = "127.0.0.1:9999"
	out, err := e.install("--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "would: wait for http://127.0.0.1:9999/healthz") {
		t.Errorf("the dry run does not use the address the pool reports:\n%s", out)
	}
}

func TestInstallUsesArm64OnRosetta(t *testing.T) {
	e := newEnv(t)
	e.vars["STUB_ARCH"] = "x86_64" // a shell under Rosetta
	e.vars["STUB_ARM64"] = "1"     // on an Apple Silicon Mac
	if out, err := e.install("--dry-run"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if i := index(e.calls(), "gh release download "); i < 0 || !strings.Contains(e.calls()[i], "*darwin_arm64*") {
		t.Fatalf("want the arm64 archive, calls: %v", e.calls())
	}
}

func TestInstallRefusesToRunAsRoot(t *testing.T) {
	for _, args := range [][]string{nil, {"--dry-run"}, {"--uninstall"}} {
		e := newEnv(t)
		e.placeInstalled()
		e.vars["STUB_UID"] = "0"
		out, err := e.install(args...)
		if err == nil || !strings.Contains(out, "do not run this as root") {
			t.Fatalf("%v: want a refusal, got %v\n%s", args, err, out)
		}
		for _, c := range e.calls() {
			if strings.HasPrefix(c, "gh ") || strings.HasPrefix(c, "launchctl ") || strings.HasPrefix(c, "pool ") {
				t.Errorf("%v: %q ran as root", args, c)
			}
		}
	}
}

// launchd can stop listing an agent whose process is still draining. The
// installer then waits until nothing answers on the health address before it
// replaces the files and loads the new pool, which could not bind it.
func TestInstallWaitsForTheOldPoolToReleaseTheAddress(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.vars["STUB_PORT_BUSY"] = "3"
	out, err := e.install()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	calls := e.calls()
	gone := index(calls, "launchctl print ")
	replaced := index(calls, "mv ")
	if gone < 0 || replaced < 0 {
		t.Fatalf("calls: %v", calls)
	}
	probes := 0
	for _, c := range calls[gone:replaced] {
		if strings.HasPrefix(c, "curl ") {
			probes++
		}
	}
	if probes != 4 {
		t.Fatalf("%d health probes before the files were replaced, want 3 answered plus 1 refused: %v", probes, calls)
	}
}

func TestInstallGivesUpWhenTheAddressStaysTaken(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	oldBin, _ := os.ReadFile(e.binPath())
	e.vars["STUB_PORT_BUSY"] = "1000000"
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "2"
	e.vars["LOCAL_CI_INSTALL_PROGRESS"] = "1"
	out, err := e.install()
	if err == nil || !strings.Contains(out, "something still answers on 127.0.0.1:8737") {
		t.Fatalf("want a refusal, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "still waiting for the old pool to release 127.0.0.1:8737") {
		t.Errorf("no progress line while waiting:\n%s", out)
	}
	if index(e.calls(), "launchctl bootstrap") >= 0 {
		t.Error("the new pool was loaded while the address was taken")
	}
	if newBin, _ := os.ReadFile(e.binPath()); !bytes.Equal(oldBin, newBin) {
		t.Error("the binary was replaced while the old pool still answered")
	}
	if strings.Contains(out, "is stopped") {
		t.Errorf("a pool still answers, so it is not stopped:\n%s", out)
	}
}

// After the load, /healthz must report the version just installed. Another
// version, or a "terminal:" line the new pool logged, is reported loudly and
// the agent is restarted once.
func TestInstallRequiresTheNewVersionOnHealthz(t *testing.T) {
	const kick = "launchctl kickstart -k gui/" + stubUID + "/" + label
	kicks := func(calls []string) int {
		n := 0
		for _, c := range calls {
			if c == kick {
				n++
			}
		}
		return n
	}
	t.Run("the new version answers", func(t *testing.T) {
		e := newEnv(t)
		out, err := e.install()
		if err != nil || !strings.Contains(out, "the pool v0.1.0 answers on http://127.0.0.1:8737/healthz") {
			t.Fatalf("%v\n%s", err, out)
		}
		if n := kicks(e.calls()); n != 0 {
			t.Fatalf("%d restarts of a healthy new pool", n)
		}
	})
	t.Run("an old version answers, a restart fixes it", func(t *testing.T) {
		e := newEnv(t)
		e.vars["STUB_NEW_VERSION"] = "v0.0.9"
		e.vars["STUB_KICKED_VERSION"] = "v0.1.0"
		out, err := e.install()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, want := range []string{"WARNING: http://127.0.0.1:8737/healthz reports version v0.0.9, not v0.1.0", "the pool v0.1.0 answers"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if n := kicks(e.calls()); n != 1 {
			t.Fatalf("%d restarts, want 1", n)
		}
	})
	t.Run("an old version keeps answering", func(t *testing.T) {
		e := newEnv(t)
		e.vars["STUB_NEW_VERSION"] = "v0.0.9"
		out, err := e.install()
		if err == nil || !strings.Contains(out, "even after a restart") {
			t.Fatalf("want a loud failure, got %v\n%s", err, out)
		}
		if n := kicks(e.calls()); n != 1 {
			t.Fatalf("%d restarts, want exactly 1", n)
		}
	})
	t.Run("the new pool logged terminal", func(t *testing.T) {
		e := newEnv(t)
		e.vars["STUB_NEW_VERSION"] = "none"
		e.vars["STUB_LOG_AT_LOAD"] = `level=ERROR msg="terminal: already running: 127.0.0.1:8737 is in use"`
		e.vars["STUB_KICKED_VERSION"] = "v0.1.0"
		out, err := e.install()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "WARNING: the new pool stopped at once:") || !strings.Contains(out, "terminal: already running") {
			t.Errorf("the terminal exit is not reported:\n%s", out)
		}
		if n := kicks(e.calls()); n != 1 {
			t.Fatalf("%d restarts, want 1", n)
		}
	})
	t.Run("a terminal line from an earlier run", func(t *testing.T) {
		e := newEnv(t)
		write(t, filepath.Join(e.home, "Library", "Logs", "local-ci-pool", "pool.log"),
			"level=ERROR msg=\"terminal: gh logged out\"\n", 0o644)
		e.vars["STUB_NEW_VERSION"] = "none" // a first start is slow to answer
		out, err := e.install()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if n := kicks(e.calls()); n != 0 || strings.Contains(out, "WARNING") {
			t.Fatalf("an old log line was taken for the new pool's (%d restarts):\n%s", n, out)
		}
		if !strings.Contains(out, "does not answer on http://127.0.0.1:8737/healthz yet") {
			t.Errorf("a slow first start is not reported:\n%s", out)
		}
	})
}

// An upgrade interrupted after the old pool was stopped must still say so.
// Without its INT and TERM traps, bash 3.2 runs the EXIT trap after a SIGTERM
// with $? = 0 (so no line), and ignores a SIGINT sent to the script alone.
func TestInstallInterruptedAfterTheStopSaysSo(t *testing.T) {
	for name, sig := range map[string]os.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": os.Interrupt} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.placeInstalled()
			e.vars["STUB_PRINT_SUCCESSES"] = "1000000"
			e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "100000"
			cmd, out := e.startInstall()
			deadline := time.Now().Add(20 * time.Second)
			for index(e.calls(), "launchctl print ") < 0 {
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					t.Fatalf("the installer never reached the wait:\n%s", out.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("the interrupted installer exited 0:\n%s", out.String())
				}
			case <-time.After(20 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("the installer did not stop on the signal")
			}
			if !strings.Contains(out.String(), strandedLine) {
				t.Errorf("output lacks the line about the stopped pool:\n%s", out.String())
			}
			if exists(e.binPath()+".new") || exists(e.plistPath()+".new") {
				t.Error("a staged file was left behind")
			}
		})
	}
}

// The provenance check refuses an archive built on a self-hosted runner when
// this gh can check that, and still works with a gh that cannot.
func TestInstallDeniesSelfHostedBuildsWhenGhCan(t *testing.T) {
	for name, tc := range map[string]struct {
		old  string
		want bool
	}{"current gh": {"0", true}, "older gh": {"1", false}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.vars["STUB_GH_OLD"] = tc.old
			if out, err := e.install("--dry-run"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			calls := e.calls()
			i := index(calls, "gh attestation verify ")
			if i < 0 {
				t.Fatalf("no verification: %v", calls)
			}
			if got := strings.Contains(calls[i], " --deny-self-hosted-runners"); got != tc.want {
				t.Fatalf("verify %q: --deny-self-hosted-runners present = %v, want %v", calls[i], got, tc.want)
			}
		})
	}
}

// The help states the uninstall order: stop and wait, then unload, then
// forget, then remove (a heartbeat written between a forget and the stop would
// survive), and that the installer sends the stop signal itself.
func TestUninstallHelpStatesTheOrder(t *testing.T) {
	e := newEnv(t)
	out, err := e.install("--help")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	flat := strings.Join(strings.Fields(out), " ")
	stop := strings.Index(flat, "stop the pool (SIGTERM, then wait until it has finished its jobs and exited")
	unload := strings.Index(flat, "unload it from launchd")
	forget := strings.Index(flat, "delete this Mac's heartbeat variables (pool forget)")
	remove := strings.Index(flat, "remove the agent and the binary")
	if stop < 0 || unload < stop || forget < unload || remove < forget {
		t.Fatalf("the help does not give stop, unload, forget, remove in order:\n%s", out)
	}
	if !strings.Contains(flat, "launchd gives an agent at most 60 seconds to exit") {
		t.Errorf("the help does not say why the installer sends the signal:\n%s", out)
	}
}

// startInstall starts install.sh like install, without waiting for it.
func (e *env) startInstall(args ...string) (*exec.Cmd, *syncBuffer) {
	e.t.Helper()
	bash, _ := exec.LookPath("bash")
	cmd := exec.Command(bash, append([]string{"install.sh"}, args...)...)
	cmd.Env = []string{
		"HOME=" + e.home,
		"PATH=" + e.stubs + ":/usr/bin:/bin",
		"STUB_LOG=" + e.log,
		"STUB_ARCHIVE_SRC=" + e.archive,
		"LOCAL_CI_INSTALL_POLL=0.05",
		"LOCAL_CI_INSTALL_HEALTH_POLL=0.05",
		"TMPDIR=" + e.t.TempDir(),
	}
	for k, v := range e.vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	return cmd, out
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// poolProcess is the script of startPool's stand-in for a running pool: it
// logs the SIGTERM it gets, then exits DRAIN_TICKS ticks of 20 ms later (a
// drain), or never when DRAIN_TICKS is negative. With DRAINING=1 it drains
// without a signal, as a pool that was already asked to stop, from the
// installer's first launchctl print on (so that it is still there when the
// installer looks). TAG names it in the log.
const poolProcess = `term=0
trap 'echo "pool-process$TAG TERM" >> "$STUB_LOG"; term=1' TERM
: > "$STUB_LOG.ready$TAG"
n=0
while :; do
	if [ "${DRAINING:-0}" = 1 ] && [ -e "$STUB_LOG.printed" ]; then
		term=1
	fi
	if [ "$term" = 1 ] && [ "$DRAIN_TICKS" -ge 0 ]; then
		if [ "$n" -ge "$DRAIN_TICKS" ]; then
			echo "pool-process$TAG exit" >> "$STUB_LOG"
			exit 0
		fi
		n=$((n + 1))
	fi
	sleep 0.02
done
`

// startPool starts a stand-in for the running pool and makes the stub
// launchctl print report its pid while it runs, as launchd does. The process
// is reaped as soon as it exits (a zombie would still answer kill -0) and
// killed when the test ends.
func (e *env) startPool(drainTicks int) {
	e.t.Helper()
	e.vars["STUB_POOL_PID"] = strconv.Itoa(e.startStandIn("", drainTicks, false))
}

// startStandIn starts a stand-in pool process tagged tag and returns its pid.
func (e *env) startStandIn(tag string, drainTicks int, draining bool) int {
	e.t.Helper()
	ready := e.log + ".ready" + tag
	_ = os.Remove(ready)
	cmd := exec.Command("/bin/sh", "-c", poolProcess)
	d := "0"
	if draining {
		d = "1"
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "STUB_LOG=" + e.log, "DRAIN_TICKS=" + strconv.Itoa(drainTicks), "DRAINING=" + d, "TAG=" + tag}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	e.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for !exists(ready) {
		if time.Now().After(deadline) {
			e.t.Fatal("the stand-in pool did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cmd.Process.Pid
}

// macOS clamps a LaunchAgent's exit timeout to 60 s, so a launchctl bootout
// of a draining pool kills it a minute later and its running jobs are lost.
// The upgrade therefore sends the pool SIGTERM itself, waits until the process
// has exited (its drain), and only then boots the agent out and replaces the
// files.
func TestUpgradeSignalsThePoolItselfBeforeTheBootout(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.startPool(15) // drains for 0.3 s, several of the installer's 50 ms polls
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "20"
	out, err := e.install()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	calls := e.calls()
	term := index(calls, "pool-process TERM")
	exited := index(calls, "pool-process exit")
	bootout := index(calls, "launchctl bootout ")
	replaced := index(calls, "mv ")
	if term < 0 || exited < term || bootout < exited || replaced < bootout {
		t.Fatalf("want SIGTERM, the pool's exit, bootout, then the rename; calls: %v", calls)
	}
	if at := index(calls, "at-bootout "); calls[at] != "at-bootout binary=present plist=present" {
		t.Errorf("a file was touched before the bootout: %v", calls)
	}
	for _, want := range []string{"sending the pool (pid " + e.vars["STUB_POOL_PID"] + ") SIGTERM", "the pool has exited"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUninstallSignalsThePoolItselfBeforeTheBootout(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.startPool(15)
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "20"
	out, err := e.install("--uninstall")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	calls := e.calls()
	term := index(calls, "pool-process TERM")
	exited := index(calls, "pool-process exit")
	bootout := index(calls, "launchctl bootout ")
	forget := index(calls, "pool forget")
	if term < 0 || exited < term || bootout < exited || forget < bootout {
		t.Fatalf("want SIGTERM, the pool's exit, bootout, then pool forget; calls: %v", calls)
	}
	if exists(e.binPath()) || exists(e.plistPath()) {
		t.Errorf("binary %v, plist %v still exist", exists(e.binPath()), exists(e.plistPath()))
	}
}

// A pool still draining at the bound is left alone: it is neither booted out
// (which would kill it within 60 s) nor replaced or removed, and the installer
// says what happened.
func TestStopGivesUpWhenThePoolKeepsDraining(t *testing.T) {
	for name, args := range map[string][]string{"upgrade": nil, "uninstall": {"--uninstall"}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.placeInstalled()
			oldBin, _ := os.ReadFile(e.binPath())
			e.startPool(-1)
			e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "2"
			e.vars["LOCAL_CI_INSTALL_PROGRESS"] = "1"
			out, err := e.install(args...)
			if err == nil || !strings.Contains(out, "is still finishing its jobs") || !strings.Contains(out, "Nothing was replaced or removed") {
				t.Fatalf("want a refusal, got %v\n%s", err, out)
			}
			if !strings.Contains(out, "still waiting for the pool to finish its jobs and exit") {
				t.Errorf("no progress line while waiting:\n%s", out)
			}
			calls := e.calls()
			if index(calls, "pool-process TERM") < 0 {
				t.Errorf("the pool was never signalled: %v", calls)
			}
			for _, c := range calls {
				if strings.HasPrefix(c, "launchctl bootout") || strings.HasPrefix(c, "mv ") || strings.HasPrefix(c, "pool forget") {
					t.Errorf("%q ran while the pool was still draining", c)
				}
			}
			if newBin, _ := os.ReadFile(e.binPath()); !bytes.Equal(oldBin, newBin) || !exists(e.plistPath()) {
				t.Error("an installed file was replaced or removed under a draining pool")
			}
			if strings.Contains(out, "is stopped") {
				t.Errorf("the pool still runs, so it is not stopped:\n%s", out)
			}
		})
	}
}

// When launchd lists the pool as running but shows no pid, the installer
// cannot signal it, says that the bootout gives it at most 60 s, and goes on.
func TestStopWarnsWhenLaunchdShowsNoPid(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.vars["STUB_RUNNING_NO_PID"] = "1"
	out, err := e.install()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "launchd reports the pool running but no pid") {
		t.Errorf("no warning:\n%s", out)
	}
	if index(e.calls(), "launchctl bootout") < 0 {
		t.Error("the agent was not booted out")
	}
}

// poolLog writes the pool's log, so that the installer can read whether the
// running pool was already asked to stop.
func (e *env) poolLog(lines ...string) {
	write(e.t, filepath.Join(e.home, "Library", "Logs", "local-ci-pool", "pool.log"), strings.Join(lines, "\n")+"\n", 0o644)
}

func startLine(pid string) string {
	attr := ""
	if pid != "" {
		attr = " pid=" + pid
	}
	return `time=2026-10-07T10:00:00.000+02:00 level=INFO msg="pool starting"` + attr + ` machine=examplemac slots=2 instance=main runner=2.338.0 probe=false`
}

const stoppingLine = `time=2026-10-07T10:05:00.000+02:00 level=INFO msg="stopping: draining every project"`

// The pool restores the default SIGTERM action after the first one, so a
// second SIGTERM kills it mid-drain. A pool already draining (asked to stop by
// an earlier run of the installer, or by hand) is therefore only waited for.
// Whether it drains is read from its log: a "stopping" line after the start
// line of this very process (by pid; a start line without a pid is v0.1.0's).
func TestStopNeverSignalsADrainingPoolTwice(t *testing.T) {
	for name, tc := range map[string]struct {
		lines    func(pid string) []string
		draining bool // the stand-in drains without a signal
		signal   bool
	}{
		"this pool is draining": {func(pid string) []string {
			return []string{startLine("1"), stoppingLine, startLine(pid), stoppingLine}
		}, true, false},
		"a v0.1.0 pool is draining": {func(string) []string { return []string{startLine(""), stoppingLine} }, true, false},
		"an earlier run drained": {func(pid string) []string {
			return []string{startLine("1"), stoppingLine, startLine(pid)}
		}, false, true},
		"another process's lines": {func(string) []string { return []string{startLine("1"), stoppingLine} }, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.placeInstalled()
			pid := strconv.Itoa(e.startStandIn("", 15, tc.draining))
			e.vars["STUB_POOL_PID"] = pid
			e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "20"
			e.poolLog(tc.lines(pid)...)
			out, err := e.install()
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			calls := e.calls()
			if got := index(calls, "pool-process TERM") >= 0; got != tc.signal {
				t.Fatalf("SIGTERM sent = %v, want %v; calls: %v\n%s", got, tc.signal, calls, out)
			}
			if exited, bootout := index(calls, "pool-process exit"), index(calls, "launchctl bootout "); exited < 0 || bootout < exited {
				t.Errorf("want the pool's exit before the bootout; calls: %v", calls)
			}
			if said := strings.Contains(out, "is already draining; waiting for it to exit without signalling it again"); said == tc.signal {
				t.Errorf("the output does not say what was done:\n%s", out)
			}
		})
	}
}

// launchd reporting another pid, or none, means the process it ran is gone:
// a pid reused by an unrelated process is not waited for, and a pool that
// launchd restarted after a non-zero exit mid-drain is not signalled but
// booted out with the agent.
func TestStopTreatsAChangedPidAsExited(t *testing.T) {
	e := newEnv(t)
	e.placeInstalled()
	e.vars["STUB_POOL_PID"] = strconv.Itoa(e.startStandIn("", -1, false)) // never exits
	e.vars["STUB_NEXT_PID"] = strconv.Itoa(e.startStandIn("-restarted", -1, false))
	e.vars["STUB_PID_PRINTS"] = "4"
	e.vars["LOCAL_CI_INSTALL_WAIT_LIMIT"] = "10"
	out, err := e.install()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	calls := e.calls()
	if index(calls, "pool-process TERM") < 0 || index(calls, "launchctl bootout ") < index(calls, "pool-process TERM") {
		t.Errorf("want SIGTERM to the pool, then the bootout; calls: %v", calls)
	}
	if index(calls, "pool-process-restarted TERM") >= 0 {
		t.Errorf("the installer signalled the restarted pool: %v", calls)
	}
	if !strings.Contains(out, "the pool has exited") {
		t.Errorf("output:\n%s", out)
	}
}

// The minutes the dry run announces for a stop are the pool's longest stop,
// its drain plus the work after it, rounded up.
func TestDryRunStatesThePoolsStopBound(t *testing.T) {
	e := newEnv(t)
	stop := poolDefaults(t).StopSeconds
	if stop <= 0 {
		t.Fatalf("print-defaults reports no stop_seconds")
	}
	want := fmt.Sprintf("it finishes its jobs first, up to %d minutes;", (stop+59)/60)
	for _, args := range [][]string{{"--dry-run"}, {"--uninstall", "--dry-run"}} {
		if args[0] == "--uninstall" {
			e.placeInstalled()
		}
		out, err := e.install(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%v: the dry run lacks %q:\n%s", args, want, out)
		}
	}
}
