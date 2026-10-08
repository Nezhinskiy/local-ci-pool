package runner

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const jitSecret = "JITSECRET-0123456789abcdef"

// stubRun is the fake run.sh: it records its argument vector and its
// environment, installs a TERM trap, reports that it is ready, and waits.
const stubRun = `#!/bin/sh
printf '%s\n' "$@" > "$OUT/argv"
env > "$OUT/env"
if ! touch "${0%/*}/_diag"; then echo unwritable > "$OUT/root_not_writable"; fi
if [ -n "${STUB_EXIT_NOW:-}" ]; then exit "$STUB_EXIT_NOW"; fi
trap 'echo TERM > "$OUT/signal"; exit 7' TERM
: > "$OUT/ready"
while :; do sleep 0.05; done
`

const stubListener = `#!/bin/sh
echo "2.338.0"
`

type rig struct {
	t     *testing.T
	dir   string
	mount string
	root  string
	out   string
	bin   string // stub directory, first on PATH
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, dir: dir, mount: filepath.Join(dir, "mount"), root: filepath.Join(dir, "root"), out: filepath.Join(dir, "out"), bin: filepath.Join(dir, "stubs")}
	for _, d := range []string{r.mount, filepath.Join(r.mount, "bin"), r.out, r.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.write(filepath.Join(r.mount, "run.sh"), stubRun, 0o755)
	r.write(filepath.Join(r.mount, "bin", "Runner.Listener"), stubListener, 0o755)
	r.ldconfig(false)
	return r
}

func (r *rig) write(path, body string, mode os.FileMode) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		r.t.Fatal(err)
	}
}

// ldconfig installs a stub `ldconfig -p` that does or does not list libicu.
func (r *rig) ldconfig(withICU bool) {
	body := "#!/bin/sh\necho '\tlibc.so.6 (libc6,AArch64) => /lib/aarch64-linux-gnu/libc.so.6'\n"
	if withICU {
		body += "echo '\tlibicuuc.so.74 (libc6,AArch64) => /lib/aarch64-linux-gnu/libicuuc.so.74'\n"
	}
	r.write(filepath.Join(r.bin, "ldconfig"), body, 0o755)
}

func (r *rig) cmd(stdin string, args ...string) *exec.Cmd {
	r.t.Helper()
	script, err := filepath.Abs("entrypoint.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command(script, args...)
	cmd.Env = []string{
		"PATH=" + r.bin + ":/usr/bin:/bin",
		"HOME=" + r.dir,
		"LOCAL_CI_MOUNT=" + r.mount,
		"LOCAL_CI_ROOT=" + r.root,
		"OUT=" + r.out,
	}
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

func (r *rig) read(name string) string {
	b, err := os.ReadFile(filepath.Join(r.out, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("not an exit error: %v", err)
	return -1
}

// startGrouped starts cmd in its own process group and kills the group when
// the test ends, so a broken entrypoint cannot leave the stub running.
func startGrouped(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestEntrypointForwardsTermAndNeverExports(t *testing.T) {
	r := newRig(t)
	cmd := r.cmd(jitSecret + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	startGrouped(t, cmd)
	waitFor(t, filepath.Join(r.out, "ready"))
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 7 {
			t.Fatalf("entrypoint exited with %d, want the stub's 7; stderr: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the entrypoint did not exit after TERM")
	}
	if got := strings.TrimSpace(r.read("signal")); got != "TERM" {
		t.Errorf("the stub's signal record = %q, want TERM", got)
	}
	env := r.read("env")
	if env == "" {
		t.Fatal("the stub recorded no environment")
	}
	if strings.Contains(env, jitSecret) {
		t.Errorf("the JIT configuration is in the stub's environment")
	}
	argv := strings.Split(strings.TrimSpace(r.read("argv")), "\n")
	if len(argv) != 2 || argv[0] != "--jitconfig" || argv[1] != jitSecret {
		t.Errorf("stub argv = %q, want --jitconfig and the configuration", argv)
	}
	if r.read("root_not_writable") != "" {
		t.Error("the runner root is not writable")
	}
}

// A shell starts a background command with SIGINT ignored, so the child could
// never see an INT. The entrypoint forwards INT as TERM instead.
func TestEntrypointForwardsIntAsTerm(t *testing.T) {
	r := newRig(t)
	cmd := r.cmd(jitSecret + "\n")
	startGrouped(t, cmd)
	waitFor(t, filepath.Join(r.out, "ready"))
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 7 {
			t.Fatalf("exit %d, want the stub's 7", code)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the entrypoint did not exit after INT")
	}
	if got := strings.TrimSpace(r.read("signal")); got != "TERM" {
		t.Errorf("signal record = %q, want TERM", got)
	}
}

func TestEntrypointReturnsTheChildStatus(t *testing.T) {
	r := newRig(t)
	cmd := r.cmd(jitSecret + "\n")
	cmd.Env = append(cmd.Env, "STUB_EXIT_NOW=0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("status 0: %v", err)
	}
	cmd = r.cmd(jitSecret + "\n")
	cmd.Env = append(cmd.Env, "STUB_EXIT_NOW=42")
	if code := exitCode(t, cmd.Run()); code != 42 {
		t.Fatalf("exit %d, want 42", code)
	}
}

func TestEntrypointReadsTheLastLineWithoutNewline(t *testing.T) {
	r := newRig(t)
	cmd := r.cmd(jitSecret) // no trailing newline
	cmd.Env = append(cmd.Env, "STUB_EXIT_NOW=0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := r.read("argv"); !strings.Contains(got, jitSecret) {
		t.Fatalf("argv = %q", got)
	}
}

func TestEntrypointNeedsAConfiguration(t *testing.T) {
	r := newRig(t)
	for _, in := range []string{"", "\n"} {
		cmd := r.cmd(in)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		code := exitCode(t, cmd.Run())
		if code == 0 || !strings.Contains(stderr.String(), "no JIT configuration") {
			t.Errorf("stdin %q: exit %d, stderr %q", in, code, stderr.String())
		}
	}
	if r.read("argv") != "" {
		t.Error("the runner started without a configuration")
	}
}

func TestEntrypointNeverStartsWithAnInheritedJitVariable(t *testing.T) {
	// If the shell variable were also set in the environment, assigning it
	// would export it. The script clears it first.
	r := newRig(t)
	cmd := r.cmd(jitSecret + "\n")
	cmd.Env = append(cmd.Env, "local_ci_jit=inherited", "STUB_EXIT_NOW=0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if env := r.read("env"); strings.Contains(env, jitSecret) || strings.Contains(env, "local_ci_jit") {
		t.Errorf("the stub's environment carries the JIT variable:\n%s", env)
	}
}

func TestEntrypointRootIsACopyNotASymlink(t *testing.T) {
	r := newRig(t)
	cmd := r.cmd(jitSecret + "\n")
	cmd.Env = append(cmd.Env, "STUB_EXIT_NOW=0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run.sh", "bin"} {
		fi, err := os.Lstat(filepath.Join(r.root, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s in the runner root is a symlink", name)
		}
	}
	fi, err := os.Stat(filepath.Join(r.root, "run.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, %v; want 0755", fi.Mode(), err)
	}
}

func TestEntrypointRootIsWritableWhenTheMountIsReadOnly(t *testing.T) {
	r := newRig(t)
	if err := os.Chmod(r.mount, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.mount, 0o755) })
	cmd := r.cmd(jitSecret + "\n")
	cmd.Env = append(cmd.Env, "STUB_EXIT_NOW=0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if r.read("root_not_writable") != "" {
		t.Error("the runner root inherited the read-only mode of the mount")
	}
}

func TestEntrypointSetsInvariantOnlyWithoutIcu(t *testing.T) {
	const invariant = "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1"
	for _, tc := range []struct {
		name    string
		withICU bool
		want    bool
	}{
		{"no libicu", false, true},
		{"libicu listed", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.ldconfig(tc.withICU)
			cmd := r.cmd(jitSecret + "\n")
			cmd.Env = append(cmd.Env, "STUB_EXIT_NOW=0")
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			env := r.read("env")
			if env == "" {
				t.Fatal("the stub recorded no environment")
			}
			if got := strings.Contains(env, invariant); got != tc.want {
				t.Errorf("invariant mode present = %v, want %v\n%s", got, tc.want, env)
			}
		})
	}
}

func TestEntrypointPreflightPrintsTheVersion(t *testing.T) {
	r := newRig(t)
	caFile := filepath.Join(r.dir, "ca.crt")
	r.write(caFile, "-----BEGIN CERTIFICATE-----\n", 0o644)
	cmd := r.cmd("", "--preflight") // no JIT on stdin: preflight never reads one
	cmd.Env = append(cmd.Env, "LOCAL_CI_CA_PATHS="+filepath.Join(r.dir, "missing")+" "+caFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "2.338.0" {
		t.Fatalf("output = %q", out)
	}
	if r.read("argv") != "" {
		t.Error("preflight ran run.sh")
	}
}

func TestEntrypointPreflightRequiresCABundle(t *testing.T) {
	r := newRig(t)
	empty := filepath.Join(r.dir, "empty.crt")
	r.write(empty, "", 0o644)
	for name, paths := range map[string]string{
		"missing":       filepath.Join(r.dir, "nope1") + " " + filepath.Join(r.dir, "nope2"),
		"empty file":    empty,
		"missing+empty": filepath.Join(r.dir, "nope1") + " " + empty,
	} {
		t.Run(name, func(t *testing.T) {
			cmd := r.cmd("", "--preflight")
			cmd.Env = append(cmd.Env, "LOCAL_CI_CA_PATHS="+paths)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			code := exitCode(t, cmd.Run())
			if code == 0 {
				t.Fatal("preflight passed without a CA bundle")
			}
			if !strings.Contains(stderr.String(), "local-ci: image has no CA bundle") {
				t.Errorf("stderr = %q", stderr.String())
			}
			if strings.Contains(stdout.String(), "2.338.0") {
				t.Error("the runner was started although the bundle is missing")
			}
		})
	}
}

func TestEntrypointPreflightPropagatesAListenerFailure(t *testing.T) {
	r := newRig(t)
	r.write(filepath.Join(r.mount, "bin", "Runner.Listener"), "#!/bin/sh\necho 'Couldn'\"'\"'t find a valid ICU package' >&2\nexit 134\n", 0o755)
	caFile := filepath.Join(r.dir, "ca.crt")
	r.write(caFile, "x\n", 0o644)
	cmd := r.cmd("", "--preflight")
	cmd.Env = append(cmd.Env, "LOCAL_CI_CA_PATHS="+caFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if code := exitCode(t, cmd.Run()); code != 134 {
		t.Fatalf("exit %d, want the listener's 134", code)
	}
	if !strings.Contains(stderr.String(), "ICU") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEntrypointPreflightSetsInvariantWithoutIcu(t *testing.T) {
	r := newRig(t)
	r.write(filepath.Join(r.mount, "bin", "Runner.Listener"), "#!/bin/sh\necho \"2.338.0 ${DOTNET_SYSTEM_GLOBALIZATION_INVARIANT:-unset}\"\n", 0o755)
	caFile := filepath.Join(r.dir, "ca.crt")
	r.write(caFile, "x\n", 0o644)
	for withICU, want := range map[bool]string{false: "2.338.0 1", true: "2.338.0 unset"} {
		r.ldconfig(withICU)
		cmd := r.cmd("", "--preflight")
		cmd.Env = append(cmd.Env, "LOCAL_CI_CA_PATHS="+caFile)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("withICU=%v: %q, %v; want %q", withICU, out, err, want)
		}
	}
}

func TestEntrypointRejectsUnknownArguments(t *testing.T) {
	r := newRig(t)
	for _, args := range [][]string{{"--bogus"}, {"--preflight", "x"}} {
		cmd := r.cmd(jitSecret+"\n", args...)
		if code := exitCode(t, cmd.Run()); code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
	}
	if r.read("argv") != "" {
		t.Error("the runner started")
	}
}
