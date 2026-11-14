package vm

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	args  string
	stdin string
}

// fakeLima records every limactl call; fail makes the call whose arguments start with it fail.
type fakeLima struct {
	mu    sync.Mutex
	calls []call
	fail  string
	list  string
}

func (f *fakeLima) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	joined := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, call{joined, in})
	f.mu.Unlock()
	if f.fail != "" && strings.HasPrefix(joined, f.fail) {
		return nil, errors.New("boom")
	}
	if args[0] == "list" {
		return []byte(f.list), nil
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

func backend(f *fakeLima, free float64) *Backend {
	return &Backend{Lima: f, CPUs: 4, MemoryGiB: 6, DiskGiB: 30, MinFreeGiB: 15, FreeGiB: func() (float64, error) { return free, nil }}
}

func TestBuildBaseBuildsAsideThenReplacesAndProtects(t *testing.T) {
	f := &fakeLima{}
	err := backend(f, 50).BuildBase(context.Background(), "alpha", strings.NewReader("TREE"), strings.NewReader("SRC"), "ci/vm/provision.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"delete --force local-ci-docker-base-alpha-building",
		"create --tty=false --plain --name=local-ci-docker-base-alpha-building --cpus=4 --memory=6 --disk=30 template:docker-rootful",
		"start --tty=false local-ci-docker-base-alpha-building",
		"shell local-ci-docker-base-alpha-building -- sudo useradd --uid 1001 --create-home --groups docker runner",
		"shell local-ci-docker-base-alpha-building -- sudo sh -c mkdir -p /opt/local-ci && tar -x -C /opt/local-ci",
		"shell local-ci-docker-base-alpha-building -- sudo sh -c mkdir -p /opt/local-ci-source && tar -x -C /opt/local-ci-source",
		"shell local-ci-docker-base-alpha-building -- sudo sh -c cd /opt/local-ci-source && test -f ci/vm/provision.sh && test ! -L ci/vm/provision.sh && bash ci/vm/provision.sh",
		"shell local-ci-docker-base-alpha-building -- sudo rm -rf /opt/local-ci-source",
		"stop local-ci-docker-base-alpha-building",
		"unprotect local-ci-docker-base-alpha",
		"delete --force local-ci-docker-base-alpha",
		"rename local-ci-docker-base-alpha-building local-ci-docker-base-alpha",
		"protect local-ci-docker-base-alpha",
	}
	if got := f.argsList(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f.calls[4].stdin != "TREE" || f.calls[5].stdin != "SRC" {
		t.Fatalf("the tree and the source must reach their own extraction: %q, %q", f.calls[4].stdin, f.calls[5].stdin)
	}
}

func TestAFailedBuildKeepsTheOldBase(t *testing.T) {
	f := &fakeLima{fail: "shell local-ci-docker-base-alpha-building -- sudo sh -c cd"}
	err := backend(f, 50).BuildBase(context.Background(), "alpha", strings.NewReader(""), strings.NewReader(""), "p.sh")
	if err == nil {
		t.Fatal("a failing provision must fail the build")
	}
	for _, c := range f.argsList() {
		if strings.Contains(c, "local-ci-docker-base-alpha ") || strings.HasSuffix(c, "local-ci-docker-base-alpha") {
			t.Fatalf("the old base was touched: %q", c)
		}
	}
	if last := f.argsList()[len(f.calls)-1]; last != "delete --force local-ci-docker-base-alpha-building" {
		t.Fatalf("the half-built base must be deleted, last call %q", last)
	}
}

func TestBuildBaseRefusesAProvisionPathThatCouldEscapeQuoting(t *testing.T) {
	for _, p := range []string{"a'b.sh", "/etc/x.sh", "../x.sh", "a\nb", "a b.sh", "a;b.sh", "$(x).sh", "-x.sh", "ci/../../x.sh"} {
		f := &fakeLima{}
		if err := backend(f, 50).BuildBase(context.Background(), "alpha", strings.NewReader(""), strings.NewReader(""), p); err == nil {
			t.Fatalf("accepted %q", p)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%q: limactl ran", p)
		}
	}
}

func TestLaunchClonesBootsRunsOnStdinThenDeletes(t *testing.T) {
	f := &fakeLima{}
	done := make(chan error, 1)
	if err := backend(f, 50).Launch(context.Background(), "alpha", "local-ci-0123456789ab", "JITCONFIG", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("done was not called")
	}
	job := "local-ci-docker-job-local-ci-0123456789ab"
	want := []string{
		"clone --tty=false local-ci-docker-base-alpha " + job,
		"start --tty=false " + job,
		"shell " + job + " -- sudo -u runner -H /opt/local-ci/entrypoint.sh",
		"delete --force " + job,
	}
	if got := f.argsList(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f.calls[2].stdin != "JITCONFIG\n" {
		t.Fatalf("the JIT configuration must go on stdin only, got %q", f.calls[2].stdin)
	}
	for _, c := range f.argsList() {
		if strings.Contains(c, "JITCONFIG") {
			t.Fatalf("the JIT configuration reached an argument: %q", c)
		}
	}
}

func TestLaunchRefusesOnAShortDiskAndClonesNothing(t *testing.T) {
	f := &fakeLima{}
	err := backend(f, 9.5).Launch(context.Background(), "alpha", "r", "JIT", func(error) {})
	if !errors.Is(err, ErrDiskShort) || !strings.Contains(err.Error(), "9.5 GiB free, needs 15") {
		t.Fatalf("err %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("limactl ran: %v", f.argsList())
	}
}

func TestAFailedBootDeletesTheClone(t *testing.T) {
	f := &fakeLima{fail: "start"}
	if err := backend(f, 50).Launch(context.Background(), "alpha", "r", "JIT", func(error) { t.Error("done called") }); err == nil {
		t.Fatal("a failed boot must fail the launch")
	}
	got := f.argsList()
	if got[len(got)-1] != "delete --force local-ci-docker-job-r" {
		t.Fatalf("the clone was not deleted: %v", got)
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

func TestSweepDeletesOnlyJobClones(t *testing.T) {
	f := &fakeLima{list: `{"name":"local-ci-docker-base-alpha","status":"Stopped"}
{"name":"local-ci-docker-job-local-ci-aa","status":"Running"}
{"name":"default","status":"Running"}
{"name":"local-ci-docker-job-local-ci-bb","status":"Stopped"}
`}
	deleted, err := backend(f, 50).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "local-ci-docker-job-local-ci-aa,local-ci-docker-job-local-ci-bb" {
		t.Fatalf("deleted %v", deleted)
	}
	for _, c := range f.argsList() {
		if strings.HasPrefix(c, "delete") && !strings.Contains(c, JobPrefix) {
			t.Fatalf("swept a VM that is no job clone: %q", c)
		}
	}
}
