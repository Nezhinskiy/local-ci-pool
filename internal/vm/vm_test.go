package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// errKilled is what exec.CommandContext's Wait returns for a process its context killed: an exit
// error, never the context's own error.
var errKilled = errors.New("signal: killed")

type call struct {
	args   string
	stdin  string
	ctxErr error // the call's context error when it ran
}

// fakeLima records every limactl call. A call whose arguments start with a key of fail fails with its
// error, and a call on a done context fails with the context's error, as exec.CommandContext would. hold
// makes the runner call wait until it is released or its context ends.
type fakeLima struct {
	mu    sync.Mutex
	calls []call
	fail  map[string]error
	list  string
	hold  chan struct{}
	// noEngine makes the base report no Docker Engine version.
	noEngine bool
	// holdDelete makes a delete wait until its context ends.
	holdDelete bool
}

func (f *fakeLima) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	joined := strings.Join(args, " ")
	if f.holdDelete && args[0] == "delete" {
		<-ctx.Done()
	}
	if f.hold != nil && strings.Contains(joined, "entrypoint.sh") {
		select {
		case <-f.hold:
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{joined, in, ctx.Err()})
	f.mu.Unlock()
	if ctx.Err() != nil {
		return nil, errKilled // as os/exec reports a process its context killed
	}
	for k, err := range f.fail {
		if strings.HasPrefix(joined, k) {
			return nil, err
		}
	}
	if args[0] == "list" {
		return []byte(f.list), nil
	}
	if strings.Contains(joined, "docker version") && !f.noEngine {
		return []byte("29.9.3\n"), nil
	}
	return nil, nil
}

func (f *fakeLima) argsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.args
	}
	return out
}

func (f *fakeLima) find(sub string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.args, sub) {
			return c, true
		}
	}
	return call{}, false
}

func backend(f Lima, free float64) *Backend {
	return &Backend{Lima: f, Instance: "main", CPUs: 4, MemoryGiB: 6, DiskGiB: 30, MinFreeGiB: 15,
		FreeGiB: func() (float64, error) { return free, nil }}
}

const (
	base     = "local-ci-docker-base-main-alpha"
	building = "local-ci-docker-new-main-alpha"
	old      = "local-ci-docker-old-main-alpha"
	job      = "local-ci-docker-job-main-local-ci-0123456789ab"
)

func build(t *testing.T, f *fakeLima) error {
	t.Helper()
	engine, err := backend(f, 50).BuildBase(context.Background(), "alpha", strings.NewReader("TREE"), strings.NewReader("SRC"), "ci/vm/provision.sh")
	if err == nil && engine != "29.9.3" {
		t.Fatalf("engine %q", engine)
	}
	return err
}

func TestBuildBaseBuildsAsideThenSwapsTheOldOneOut(t *testing.T) {
	f := &fakeLima{list: `{"name":"` + base + `"}`}
	if err := build(t, f); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"delete --force " + building,
		"create --tty=false --plain --name=" + building + " --cpus=4 --memory=6 --disk=30 template:docker-rootful",
		"start --tty=false " + building,
		"shell " + building + " -- sudo useradd --uid 1001 --create-home --groups docker runner",
		"shell " + building + ` -- sudo sh -c mkdir -p "$1" && tar -x -C "$1" sh /opt/local-ci`,
		"shell " + building + ` -- sudo sh -c mkdir -p "$1" && tar -x -C "$1" sh /opt/local-ci-source`,
		"shell " + building + " -- sudo sh -c " + provisionScript + " sh /opt/local-ci-source ci/vm/provision.sh",
		"shell " + building + " -- sudo rm -rf /opt/local-ci-source",
		"shell " + building + " -- sudo docker version --format {{.Server.Version}}",
		"stop " + building,
		"list --format json",
		"delete --force " + old,
		"unprotect " + base,
		"rename " + base + " " + old,
		"rename " + building + " " + base,
		"protect " + base,
		"delete --force " + old,
	}
	if got := f.argsList(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	f.mu.Lock()
	tree, src := f.calls[4].stdin, f.calls[5].stdin
	f.mu.Unlock()
	if tree != "TREE" || src != "SRC" {
		t.Fatalf("the tree and the source must reach their own extraction: %q, %q", tree, src)
	}
}

func TestAFirstBuildHasNoOldBaseToSetAside(t *testing.T) {
	f := &fakeLima{}
	if err := build(t, f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.argsList() {
		if c == "rename "+base+" "+old || strings.HasPrefix(c, "unprotect") {
			t.Fatalf("set aside an old base that does not exist: %q", c)
		}
	}
}

func TestAFailedProvisionKeepsTheOldBase(t *testing.T) {
	f := &fakeLima{list: `{"name":"` + base + `"}`, fail: map[string]error{"shell " + building + " -- sudo sh -c cd": errors.New("boom")}}
	if err := build(t, f); err == nil {
		t.Fatal("a failing provision must fail the build")
	}
	for _, c := range f.argsList() {
		if strings.Contains(c, base) {
			t.Fatalf("the old base was touched: %q", c)
		}
	}
	if got := f.argsList(); got[len(got)-1] != "delete --force "+building {
		t.Fatalf("the half-built base must be deleted, last call %q", got[len(got)-1])
	}
}

func TestAFailedSwapPutsTheOldBaseBack(t *testing.T) {
	f := &fakeLima{list: `{"name":"` + base + `"}`, fail: map[string]error{"rename " + building: errors.New("boom")}}
	if err := build(t, f); err == nil {
		t.Fatal("a failed rename must fail the build")
	}
	got := f.argsList()
	if tail := strings.Join(got[len(got)-3:], "\n"); tail != "rename "+old+" "+base+"\nprotect "+base+"\ndelete --force "+building {
		t.Fatalf("the old base was not put back and protected, or the build not deleted:\n%s", strings.Join(got, "\n"))
	}
	at := slices.Index(got, "rename "+building+" "+base)
	for _, c := range got[at:] {
		if c == "delete --force "+old {
			t.Fatalf("the old base was deleted after the failed swap: %v", got)
		}
	}

}

// cancelAfter cancels the build's context right after the call starting with after.
type cancelAfter struct {
	f      *fakeLima
	after  string
	cancel context.CancelFunc
}

func (c cancelAfter) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	out, err := c.f.Run(ctx, stdin, args...)
	if strings.HasPrefix(strings.Join(args, " "), c.after) {
		c.cancel()
	}
	return out, err
}

func TestTheSwapRunsToItsEndWhenTheBuildIsCancelled(t *testing.T) {
	f := &fakeLima{list: `{"name":"` + base + `"}`}
	ctx, cancel := context.WithCancel(context.Background())
	b := backend(cancelAfter{f, "stop " + building, cancel}, 50)
	if _, err := b.BuildBase(ctx, "alpha", strings.NewReader(""), strings.NewReader(""), "p.sh"); err != nil {
		t.Fatalf("the swap must finish after a cancel: %v", err)
	}
	if c, ok := f.find("protect " + base); !ok || c.ctxErr != nil {
		t.Fatalf("the swap ran on the cancelled context: %+v", c)
	}
}

func TestBuildBaseRefusesWhatCouldEscapeOrCollide(t *testing.T) {
	for _, p := range []string{"a'b.sh", "/etc/x.sh", "../x.sh", "a\nb", "a b.sh", "a;b.sh", "$(x).sh", "-x.sh", "ci/../../x.sh"} {
		f := &fakeLima{}
		if _, err := backend(f, 50).BuildBase(context.Background(), "alpha", strings.NewReader(""), strings.NewReader(""), p); err == nil {
			t.Fatalf("accepted provision %q", p)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%q: limactl ran", p)
		}
	}
	for _, id := range []string{"Alpha", "a_b", strings.Repeat("a", 30)} {
		f := &fakeLima{}
		if _, err := backend(f, 50).BuildBase(context.Background(), id, strings.NewReader(""), strings.NewReader(""), "p.sh"); err == nil || len(f.calls) != 0 {
			t.Fatalf("identity %q: err %v, calls %v", id, err, f.argsList())
		}
	}
	// A build's name is never another identity's base: "alpha-building" builds as another kind.
	if b, _ := backend(&fakeLima{}, 50).BaseName("alpha-building"); b == building {
		t.Fatal("a build's name collides with another identity's base")
	}
}

func TestBuildBaseRefusesOnAShortDisk(t *testing.T) {
	f := &fakeLima{}
	_, err := backend(f, 9).BuildBase(context.Background(), "alpha", strings.NewReader(""), strings.NewReader(""), "p.sh")
	// Only the leftover build is deleted first, so a leak cannot block the rebuild that clears it.
	if got := f.argsList(); !errors.Is(err, ErrDiskShort) || len(got) != 1 || got[0] != "delete --force "+building {
		t.Fatalf("err %v, calls %v", err, f.argsList())
	}
}

func TestLaunchClonesBootsRunsOnStdinThenDeletes(t *testing.T) {
	f := &fakeLima{}
	done := make(chan error, 1)
	if err := backend(f, 50).Launch(context.Background(), "alpha", "local-ci-0123456789ab", "JITCONFIG", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Fatalf("a clean exit must give done(nil), got %v", err)
	}
	want := []string{
		"clone --tty=false " + base + " " + job,
		"start --tty=false " + job,
		"shell " + job + " -- sudo -u runner -H /opt/local-ci/entrypoint.sh",
		"delete --force " + job,
	}
	if got := f.argsList(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c, _ := f.find("entrypoint.sh"); c.stdin != "JITCONFIG\n" {
		t.Fatalf("the JIT configuration must go on stdin only, got %q", c.stdin)
	}
}

func TestTheRunnersErrorReachesDone(t *testing.T) {
	f := &fakeLima{fail: map[string]error{"shell " + job: errors.New("runner exited 1")}}
	done := make(chan error, 1)
	if err := backend(f, 50).Launch(context.Background(), "alpha", "local-ci-0123456789ab", "JIT", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err == nil || !strings.Contains(err.Error(), "runner exited 1") {
		t.Fatalf("done got %v", err)
	}
	if got := f.argsList(); got[len(got)-1] != "delete --force "+job {
		t.Fatalf("a failed runner's clone must be deleted too: %v", got)
	}
}

func TestADeleteErrorReachesDone(t *testing.T) {
	f := &fakeLima{fail: map[string]error{"delete": errors.New("delete failed")}}
	done := make(chan error, 1)
	if err := backend(f, 50).Launch(context.Background(), "alpha", "local-ci-0123456789ab", "JIT", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("a clone whose delete failed must not look like a clean exit: %v", err)
	}
}

func TestTheJITNeverReachesAnArgumentAnErrorOrALog(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeLima{fail: map[string]error{"shell " + job: errors.New("runner exited 1"), "delete": errors.New("delete hung")}}
	b := backend(f, 50)
	b.Log = slog.New(slog.NewTextHandler(&logs, nil))
	done := make(chan error, 1)
	if err := b.Launch(context.Background(), "alpha", "local-ci-0123456789ab", "SECRET-JIT", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	err := wait(t, done)
	if err == nil || strings.Contains(err.Error(), "SECRET-JIT") || strings.Contains(logs.String(), "SECRET-JIT") || logs.Len() == 0 {
		t.Fatalf("err %v, logs %q", err, logs.String())
	}
	for _, c := range f.argsList() {
		if strings.Contains(c, "SECRET-JIT") {
			t.Fatalf("the JIT reached an argument: %q", c)
		}
	}
}

func TestTheJobOutlivesTheLaunchContextButNotItsDeadline(t *testing.T) {
	f := &fakeLima{hold: make(chan struct{})}
	b := backend(f, 50)
	b.JobTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	if err := b.Launch(ctx, "alpha", "local-ci-0123456789ab", "JIT", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	cancel() // the scaler's launch context ends: the job must go on
	start := time.Now()
	err := wait(t, done)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("the job must end by its own deadline, not the launch's cancel: %v after %v", err, time.Since(start))
	}
	if c, _ := f.find("delete --force " + job); c.ctxErr != nil {
		t.Fatalf("the clone's delete ran on a cancelled context: %v", c.ctxErr)
	}
}

func TestLaunchRefusesOnAShortOrUnreadableDisk(t *testing.T) {
	f := &fakeLima{}
	err := backend(f, 9.5).Launch(context.Background(), "alpha", "r", "JIT", func(error) {})
	if !errors.Is(err, ErrDiskShort) || !strings.Contains(err.Error(), "9.5 GiB free, needs 15") {
		t.Fatalf("err %v", err)
	}
	b := backend(f, 50)
	b.FreeGiB = func() (float64, error) { return 0, errors.New("statfs") }
	if err := b.Launch(context.Background(), "alpha", "r", "JIT", func(error) {}); err == nil {
		t.Fatal("an unreadable disk must refuse")
	}
	if len(f.calls) != 0 {
		t.Fatalf("limactl ran: %v", f.argsList())
	}
}

func TestAFailedCloneOrBootDeletesTheClone(t *testing.T) {
	for _, step := range []string{"clone", "start"} {
		f := &fakeLima{fail: map[string]error{step: errors.New("boom")}}
		if err := backend(f, 50).Launch(context.Background(), "alpha", "r", "JIT", func(error) { t.Error("done called") }); err == nil {
			t.Fatalf("%s: a failure must fail the launch", step)
		}
		if got := f.argsList(); got[len(got)-1] != "delete --force local-ci-docker-job-main-r" {
			t.Fatalf("%s: the clone was not deleted: %v", step, got)
		}
	}
}

func TestLaunchRefusesAJITThatIsNotOneLine(t *testing.T) {
	for _, jit := range []string{"", "a\nb", "a\r"} {
		f := &fakeLima{}
		if err := backend(f, 50).Launch(context.Background(), "alpha", "r", jit, func(error) {}); err == nil || len(f.calls) != 0 {
			t.Fatalf("%q: err %v, calls %v", jit, err, f.argsList())
		}
	}
}

func TestRemoveDeletesTheClone(t *testing.T) {
	f := &fakeLima{}
	if err := backend(f, 50).Remove(context.Background(), "local-ci-0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if got := f.argsList(); len(got) != 1 || got[0] != "delete --force "+job {
		t.Fatalf("calls %v", got)
	}
}

func TestSweepKeepsToItsInstanceAndResolvesAnInterruptedSwap(t *testing.T) {
	f := &fakeLima{list: `{"name":"local-ci-docker-base-main-alpha"}
{"name":"local-ci-docker-job-main-local-ci-aa"}
{"name":"local-ci-docker-job-probe-x-local-ci-bb"}
{"name":"local-ci-docker-new-main-beta"}
{"name":"local-ci-docker-old-main-alpha"}
{"name":"local-ci-docker-old-main-gamma"}
{"name":"default"}
`}
	deleted, err := backend(f, 50).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"local-ci-docker-job-main-local-ci-aa": true, "local-ci-docker-new-main-beta": true, "local-ci-docker-old-main-alpha": true}
	if len(deleted) != len(want) {
		t.Fatalf("deleted %v", deleted)
	}
	for _, d := range deleted {
		if !want[d] {
			t.Fatalf("deleted %q", d)
		}
	}
	if _, ok := f.find("rename local-ci-docker-old-main-gamma local-ci-docker-base-main-gamma"); !ok {
		t.Fatalf("an old base with no base in place must be put back: %v", f.argsList())
	}
	if _, ok := f.find("protect local-ci-docker-base-main-gamma"); !ok {
		t.Fatalf("a base put back must be protected: %v", f.argsList())
	}
	if _, ok := f.find("protect local-ci-docker-base-main-alpha"); !ok {
		t.Fatalf("every base of the instance is protected again: %v", f.argsList())
	}
	for _, c := range f.argsList() {
		if strings.Contains(c, "probe-x") || strings.Contains(c, "default") || c == "delete --force local-ci-docker-base-main-alpha" {
			t.Fatalf("swept outside this instance's leftovers: %q", c)
		}
	}
}

func TestSweepAndSwapFailOnAnUnreadableList(t *testing.T) {
	if _, err := backend(&fakeLima{list: "not json\n"}, 50).Sweep(context.Background()); err == nil {
		t.Fatal("a garbled list must fail the sweep")
	}
	f := &fakeLima{fail: map[string]error{"list": errors.New("boom")}}
	if _, err := backend(f, 50).Sweep(context.Background()); err == nil {
		t.Fatal("a failed list must fail the sweep")
	}
	if err := build(t, f); err == nil {
		t.Fatal("a failed list must fail the swap")
	}
}

func wait(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("done was not called")
		return nil
	}
}

func TestABaseWithNoEngineVersionFailsAndKeepsTheOldOne(t *testing.T) {
	f := &fakeLima{list: `{"name":"` + base + `"}`, noEngine: true}
	if err := build(t, f); err == nil || !strings.Contains(err.Error(), "no Docker Engine version") {
		t.Fatalf("err %v", err)
	}
	for _, c := range f.argsList() {
		if strings.Contains(c, base) {
			t.Fatalf("the old base was touched: %q", c)
		}
	}
}

func TestAFailedSetAsideKeepsTheOldBaseProtected(t *testing.T) {
	for _, step := range []string{"unprotect " + base, "rename " + base} {
		f := &fakeLima{list: `{"name":"` + base + `"}`, fail: map[string]error{step: errors.New("boom")}}
		if err := build(t, f); err == nil {
			t.Fatalf("%s: a failed set-aside must fail the build", step)
		}
		got := f.argsList()
		if slices.Contains(got, "rename "+building+" "+base) {
			t.Fatalf("%s: the build was renamed in over a base still in place: %v", step, got)
		}
		if step != "unprotect "+base && !slices.Contains(got, "protect "+base) {
			t.Fatalf("%s: the old base was left unprotected: %v", step, got)
		}
		if got[len(got)-1] != "delete --force "+building {
			t.Fatalf("%s: the build was not deleted: %v", step, got)
		}
	}
}

func TestAFailedFinalProtectStillServesTheNewBase(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeLima{list: `{"name":"` + base + `"}`, fail: map[string]error{"protect " + base: errors.New("boom")}}
	b := backend(f, 50)
	b.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := b.BuildBase(context.Background(), "alpha", strings.NewReader(""), strings.NewReader(""), "p.sh"); err != nil {
		t.Fatalf("the new base serves: a failed protect is no failed build: %v", err)
	}
	if got := f.argsList(); got[len(got)-1] != "delete --force "+old || !strings.Contains(logs.String(), "protecting a docker base failed") {
		t.Fatalf("the old base must still go, and the failure be logged: %v %q", got, logs.String())
	}
}

func TestCleanupAfterACancelRunsOnALiveContext(t *testing.T) {
	f := &fakeLima{}
	ctx, cancel := context.WithCancel(context.Background())
	b := backend(cancelAfter{f, "start --tty=false " + building, cancel}, 50)
	if _, err := b.BuildBase(ctx, "alpha", strings.NewReader(""), strings.NewReader(""), "p.sh"); err == nil {
		t.Fatal("a build cancelled half-way must fail")
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last.args != "delete --force "+building || last.ctxErr != nil {
		t.Fatalf("the half-built base was not deleted on a live context: %+v", last)
	}
	f2 := &fakeLima{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	if err := backend(cancelAfter{f2, "clone", cancel2}, 50).Launch(ctx2, "alpha", "r", "JIT", func(error) {}); err == nil {
		t.Fatal("a launch cancelled after its clone must fail")
	}
	if c, _ := f2.find("delete --force local-ci-docker-job-main-r"); c.args == "" || c.ctxErr != nil {
		t.Fatalf("the clone was not deleted on a live context: %+v", c)
	}
}

func TestADeleteIsBounded(t *testing.T) {
	f := &fakeLima{holdDelete: true}
	b := backend(f, 50)
	b.DeleteTimeout = 100 * time.Millisecond
	start := time.Now()
	if err := b.Remove(context.Background(), "r"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a hung delete must end at its timeout: %v after %v", err, time.Since(start))
	}
}

func TestTheDiskThresholdIsInclusive(t *testing.T) {
	f := &fakeLima{}
	if err := backend(f, 15).Launch(context.Background(), "alpha", "r", "JIT", func(error) {}); err != nil {
		t.Fatalf("exactly MinFreeGiB free must pass: %v", err)
	}
}

func TestTheInstanceIsValidated(t *testing.T) {
	b := backend(&fakeLima{}, 50)
	for _, inst := range []string{"Main", "a_b", ""} {
		b.Instance = inst
		if _, err := b.BaseName("alpha"); err == nil {
			t.Fatalf("instance %q accepted", inst)
		}
	}
}
