package supervisor

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
	"github.com/Nezhinskiy/local-ci-pool/internal/image"
	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
	"github.com/Nezhinskiy/local-ci-pool/internal/vm"
)

const (
	alphaDockerSet = "alpha-docker-examplemac"
	betaDockerSet  = "beta-docker-examplemac"
)

func dockerMarker() string {
	return `{"image":"example.invalid/job@` + testDigest + `","docker":{"provision":"ci/vm/provision.sh","inputs":["ci/vm"]}}`
}

// ---- fake limactl ----

// fakeLima is limactl: it records every call, and a job clone's runner (its
// "shell ... entrypoint.sh") runs until finish or a delete of the clone ends
// it.
type fakeLima struct {
	mu      sync.Mutex
	calls   []string
	stdin   map[string]string // the stdin of each call, by its arguments
	missing bool              // limactl is not installed
	fail    string            // a call whose arguments start with it fails
	list    string
	jobs    map[string]chan error // running job clones, by clone name
}

func newFakeLima() *fakeLima {
	return &fakeLima{stdin: map[string]string{}, jobs: map[string]chan error{}}
}

func (f *fakeLima) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	joined := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.stdin[joined] = in
	if f.missing {
		f.mu.Unlock()
		return nil, &exec.Error{Name: "limactl", Err: exec.ErrNotFound}
	}
	if f.fail != "" && strings.HasPrefix(joined, f.fail) {
		f.mu.Unlock()
		return nil, errors.New("limactl failed")
	}
	switch {
	case args[0] == "--version":
		f.mu.Unlock()
		return []byte("limactl version 2.3.0\n"), nil
	case args[0] == "list":
		defer f.mu.Unlock()
		return []byte(f.list), nil
	case args[0] == "delete":
		if ch := f.jobs[args[len(args)-1]]; ch != nil {
			delete(f.jobs, args[len(args)-1])
			ch <- errors.New("the VM is gone")
		}
	case args[0] == "shell" && strings.HasPrefix(args[1], vm.JobPrefix):
		ch := make(chan error, 1)
		f.jobs[args[1]] = ch
		f.mu.Unlock()
		select {
		case err := <-ch:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Unlock()
	return nil, nil
}

// running lists the runners whose clone runs, sorted.
func (f *fakeLima) running() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for job := range f.jobs {
		out = append(out, strings.TrimPrefix(job, vm.JobPrefix))
	}
	slices.Sort(out)
	return out
}

// finish ends a runner's job: its clone's runner exits cleanly.
func (f *fakeLima) finish(runner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch := f.jobs[vm.JobName(runner)]; ch != nil {
		delete(f.jobs, vm.JobName(runner))
		ch <- nil
	}
}

func (f *fakeLima) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeLima) set(mut func(f *fakeLima)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mut(f)
}

func (f *fakeLima) input(call string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin[call]
}

func (f *fakeLima) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// ---- fake mirror ----

// fakeMirror resolves a ref to "commit-<ref>" and archives a commit as
// "archive of <commit>"; digest is the inputs' digest at every commit.
type fakeMirror struct {
	mu      sync.Mutex
	digest  string
	fetched []string
}

func (m *fakeMirror) Dir(repo string) string { return "/mirror/" + repo }

func (m *fakeMirror) Fetch(_ context.Context, repo, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetched = append(m.fetched, repo+"@"+ref)
	return "commit-" + ref, nil
}

func (m *fakeMirror) InputsDigest(context.Context, string, string, []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.digest, nil
}

func (m *fakeMirror) Archive(_ context.Context, _, commit string, _ []string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("archive of " + commit)), nil
}

func (m *fakeMirror) setDigest(d string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.digest = d
}

// ---- the runner tree, from the fake Docker ----

func (f *fakeDocker) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	var res client.ImageInspectResult
	res.ID = "sha256:tree-of-" + ref
	return res, nil
}

func (f *fakeDocker) ContainerExport(context.Context, string, client.ContainerExportOptions) (client.ContainerExportResult, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "entrypoint.sh", Mode: 0o755, Size: 4})
	_, _ = tw.Write([]byte("TREE"))
	_ = tw.Close()
	return io.NopCloser(&buf), nil
}

// ---- harness ----

// withDocker gives the harness a docker backend and serves repos with a
// docker block in their marker.
func (h *harness) withDocker(repos ...string) {
	h.lima, h.mirror, h.freeGiB, h.stateDir = newFakeLima(), &fakeMirror{digest: "d1"}, 50, h.t.TempDir()
	for _, r := range repos {
		h.gh.repos["example/"+r].marker = dockerMarker()
	}
}

func (h *harness) vmBackend() *vm.Backend {
	if h.lima == nil {
		return nil
	}
	return &vm.Backend{Lima: h.lima, CPUs: 4, MemoryGiB: 6, DiskGiB: 30, MinFreeGiB: 15, FreeGiB: func() (float64, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.freeGiB, nil
	}}
}

func (h *harness) mirrorDep() image.Mirror {
	if h.mirror == nil {
		return nil
	}
	return h.mirror
}

func (h *harness) dockerHealth(repo string) (ProjectHealth, bool) {
	for _, p := range h.sup.Snapshot().Projects {
		if p.Repo == repo && p.DockerClass {
			return p, true
		}
	}
	return ProjectHealth{}, false
}

func (h *harness) dockerHealthy(repo string) bool {
	p, ok := h.dockerHealth(repo)
	return ok && p.Healthy
}

// ---- tests ----

// A repository with a docker block gets a second scale set, labelled
// <identity>-docker, next to its container one; its base is built once; a VM
// left by a killed pool is deleted at start; and its job runs in a clone of
// the base, holding the docker slot and no container slot, outside the
// budget, and uncounted against the container class's survivors.
func TestDockerClassServesASecondScaleSet(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.withDocker("alpha")
	b := newFakeBudget(true)
	h.budget = b
	h.lima.list = `{"name":"local-ci-docker-base-alpha"}` + "\n" + `{"name":"local-ci-docker-job-local-ci-0badc0ffee00"}` + "\n"
	survivor := instanceLabels("main", "local-ci-5u7v1v0r0000")
	survivor[runner.LabelProject] = "alpha"
	h.docker.addIn("local-ci-5u7v1v0r0000", survivor, container.StateRunning)
	h.start()
	h.eventually("both classes healthy", func() bool { return h.healthy(alphaRepo) && h.dockerHealthy(alphaRepo) })

	ss := h.actions.ScaleSet(alphaDockerSet)
	if ss == nil || !slices.Equal(labelNames(ss), []string{alphaDockerSet, "alpha-docker"}) {
		t.Fatalf("docker scale set %+v", ss)
	}
	if !h.logged("deleted a docker VM left by a previous run") || h.lima.count("delete --force local-ci-docker-job-local-ci-0badc0ffee00") != 1 {
		t.Fatalf("the leftover job VM was not swept: %v", h.lima.all())
	}
	if n := h.lima.count("create --tty=false --plain --name=local-ci-docker-base-alpha-building"); n != 1 {
		t.Fatalf("%d base builds, want 1", n)
	}

	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("a docker VM runs", func() bool { return len(h.lima.running()) == 1 })
	r := h.lima.running()[0]
	if got := h.lima.count("clone --tty=false local-ci-docker-base-alpha " + vm.JobName(r)); got != 1 {
		t.Fatalf("the runner's clone: %v", h.lima.all())
	}
	if s := h.sup.Snapshot(); s.InUse != 1 || h.sup.vmSlots.InUse() != 1 {
		t.Fatalf("container slots in use %d, docker slots in use %d; want the survivor's alone and 1", s.InUse, h.sup.vmSlots.InUse())
	}
	if got := h.docker.created(h.imageOf("alpha")); len(got) != 0 {
		t.Fatalf("a docker job started containers %v", got)
	}
	if got := b.admitted(); len(got) != 0 {
		t.Fatalf("the budget admitted %+v; the docker class is never gated", got)
	}
	h.actions.Started(alphaDockerSet, r)
	h.actions.Completed(alphaDockerSet, r)
	h.delivered(alphaDockerSet)
	h.lima.finish(r)
	h.eventually("the docker slot freed", func() bool { return h.sup.vmSlots.InUse() == 0 })
}

// One docker VM runs at a time on the machine, whatever the repository: beta's
// waits for alpha's to end, and its end, the Launch's done callback, is what
// frees the slot.
func TestOneDockerVMAtATime(t *testing.T) {
	h := newHarness(t, 4, 1, "alpha", "beta") // wall speed: no watchdog reconcile stands in for the exit
	h.withDocker("alpha", "beta")
	h.start()
	h.eventually("both docker classes healthy", func() bool { return h.dockerHealthy(alphaRepo) && h.dockerHealthy(betaRepo) })

	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("alpha's VM runs", func() bool { return len(h.lima.running()) == 1 })
	alpha := h.lima.running()[0]
	h.eventually("beta advertises 0", func() bool { return h.actions.Capacity(betaDockerSet) == "0" })
	h.actions.IgnoreCapacity(betaDockerSet)
	h.actions.Assign(betaDockerSet, 1)
	h.delivered(betaDockerSet)
	time.Sleep(100 * time.Millisecond)
	if got := h.lima.running(); len(got) != 1 {
		t.Fatalf("VMs running %v, want alpha's alone", got)
	}
	// alpha's job runs, so no reap can end its VM; then it completes.
	h.actions.Started(alphaDockerSet, alpha)
	h.actions.Completed(alphaDockerSet, alpha)
	h.delivered(alphaDockerSet)
	time.Sleep(100 * time.Millisecond)
	if got := h.lima.running(); len(got) != 1 || got[0] != alpha {
		t.Fatalf("VMs running %v before alpha's ended, want alpha's alone", got)
	}
	h.lima.finish(alpha)
	h.eventually("beta's VM runs after alpha's ended", func() bool {
		got := h.lima.running()
		return len(got) == 1 && got[0] != alpha
	})
}

// The reconcile compares runners with containers; a docker runner has none,
// and keeps its slot through it. A docker VM alone keeps the Mac awake.
func TestReconcileLeavesDockerRunners(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // the watchdog's reconcile every 30 ms
	h.withDocker("alpha")
	h.start()
	h.eventually("alpha's docker class healthy", func() bool { return h.dockerHealthy(alphaRepo) })
	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("a docker VM runs", func() bool { return len(h.lima.running()) == 1 })
	h.eventually("caffeinate running", func() bool { _, _, running := h.caff.state(); return running })
	time.Sleep(300 * time.Millisecond) // ten reconciles
	if n := h.sup.vmSlots.InUse(); n != 1 || len(h.lima.running()) != 1 {
		t.Fatalf("docker slots in use %d, VMs %v: the reconcile released a docker runner", n, h.lima.running())
	}
}

// An idle docker runner beyond the assigned jobs is reaped by deleting its VM,
// not a container.
func TestIdleDockerRunnerIsReapedByDeletingItsVM(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.withDocker("alpha")
	h.start()
	h.eventually("alpha's docker class healthy", func() bool { return h.dockerHealthy(alphaRepo) })
	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("a docker VM runs", func() bool { return len(h.lima.running()) == 1 })
	r := h.lima.running()[0]
	h.actions.Assign(alphaDockerSet, -1)
	h.eventually("its VM deleted and its slot freed", func() bool {
		return h.lima.count("delete --force "+vm.JobName(r)) > 0 && h.sup.vmSlots.InUse() == 0
	})
}

// Probe mode serves no docker class, sweeps no VM, and touches no limactl;
// nor does a marker read at a ref other than the default branch.
func TestNoDockerClassInProbeModeOrAtAnotherRef(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg   func(c *Config)
		calls []string // the start-up sweep's, outside probe mode
	}{
		"probe":      {func(c *Config) { c.Probe, c.OnlyRepo = true, alphaRepo }, nil},
		"marker ref": {func(c *Config) { c.MarkerRef = "feature" }, []string{"list --format json"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 2, 1000, "alpha")
			h.withDocker("alpha")
			tc.cfg(&h.cfg)
			h.start()
			h.eventually("alpha healthy", func() bool { return h.healthy(alphaRepo) })
			time.Sleep(100 * time.Millisecond) // two discovery passes
			if _, ok := h.dockerHealth(alphaRepo); ok || h.actions.ScaleSet(alphaDockerSet) != nil {
				t.Fatal("a docker class was served")
			}
			if calls := h.lima.all(); !slices.Equal(calls, tc.calls) {
				t.Fatalf("limactl calls %v, want %v", calls, tc.calls)
			}
		})
	}
}

// Without limactl the docker class is unhealthy with the reason; the container
// class is served as ever.
func TestDockerClassWithoutLimactl(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.withDocker("alpha")
	h.lima.missing = true
	h.start()
	h.eventually("alpha healthy, its docker class not", func() bool {
		p, ok := h.dockerHealth(alphaRepo)
		return h.healthy(alphaRepo) && ok && p.Class == "setup" && p.Reason == "docker class: limactl not found"
	})
	if h.actions.ScaleSet(alphaDockerSet) != nil {
		t.Fatal("a docker scale set without limactl")
	}
}

// Below the free-disk threshold a launch is refused: the runner is
// unregistered, the slot freed, the reason says it, and the job stays queued.
func TestDockerLaunchShortOfDisk(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha")
	h.withDocker("alpha")
	h.freeGiB = 5
	h.start()
	h.eventually("alpha's docker class healthy", func() bool { return h.dockerHealthy(alphaRepo) })
	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("unhealthy for the disk", func() bool {
		p, _ := h.dockerHealth(alphaRepo)
		return p.Class == "vm" && strings.HasPrefix(p.Reason, "docker vm: "+vm.ErrDiskShort.Error()+": 5.0 GiB free, needs 15")
	})
	h.eventually("no runner left registered, no slot held", func() bool {
		return len(h.actions.Runners(alphaDockerSet)) == 0 && h.sup.vmSlots.InUse() == 0
	})
	if n := h.lima.count("clone"); n != 0 {
		t.Fatalf("%d clones below the threshold", n)
	}
	h.mu.Lock()
	h.freeGiB = 50
	h.mu.Unlock()
	h.eventually("a VM once the disk is free", func() bool { return len(h.lima.running()) == 1 })
	// The refused starts count as runners gone without a job, as any failed
	// start does, until the job starts.
	h.actions.Started(alphaDockerSet, h.lima.running()[0])
	h.eventually("healthy again", func() bool { return h.dockerHealthy(alphaRepo) })
}

// A rebuild that fails keeps the old base, and so the runners, and marks the
// docker class unhealthy with the reason.
func TestFailedDockerRebuildKeepsServing(t *testing.T) {
	h := newHarness(t, 2, 1000, "alpha") // a discovery pass every 50 ms
	h.withDocker("alpha")
	h.start()
	h.eventually("alpha's docker class healthy", func() bool { return h.dockerHealthy(alphaRepo) })
	h.lima.set(func(f *fakeLima) { f.fail = "shell local-ci-docker-base-alpha-building -- sudo sh -c cd" })
	h.mirror.setDigest("d2")
	h.eventually("unhealthy for the base", func() bool {
		p, _ := h.dockerHealth(alphaRepo)
		return p.Class == "image" && strings.HasPrefix(p.Reason, "docker base: building the docker base of alpha")
	})
	h.actions.Assign(alphaDockerSet, 1)
	h.eventually("its job still runs on the old base", func() bool { return len(h.lima.running()) == 1 })
}

// The base is built from the default branch's archive, and the block's inputs
// are read there, whatever ref the marker came from; an unchanged key builds
// nothing, a changed one builds again.
func TestDockerBaseIsBuiltFromTheDefaultBranch(t *testing.T) {
	lima, mirror := newFakeLima(), &fakeMirror{digest: "d1"}
	s := New(Config{}, Deps{
		Docker: newFakeDocker(), Mirror: mirror, StateDir: t.TempDir(),
		VM: &vm.Backend{Lima: lima, CPUs: 4, MemoryGiB: 6, DiskGiB: 30},
	})
	pr := &project{s: s, identity: "alpha", log: s.log}
	p := discovery.Project{Identity: "alpha", Repo: alphaRepo, Ref: "feature", DefaultBranch: "main",
		Marker: discovery.Marker{Docker: &discovery.DockerVM{Provision: "ci/vm/provision.sh", Inputs: []string{"ci/vm"}}}}
	m := runnermount.Mount{Version: "2.338.0", Spec: mount.Mount{Type: mount.TypeImage, Source: runnermount.Ref("2.338.0")}}
	extract := "shell local-ci-docker-base-alpha-building -- sudo sh -c mkdir -p " + vm.SourceDir + " && tar -x -C " + vm.SourceDir
	tree := "shell local-ci-docker-base-alpha-building -- sudo sh -c mkdir -p " + vm.TreeDir + " && tar -x -C " + vm.TreeDir
	create := "create --tty=false --plain --name=local-ci-docker-base-alpha-building"

	img, err := pr.ensureBase(context.Background(), p, m)
	if err != nil {
		t.Fatal(err)
	}
	if got := lima.input(extract); got != "archive of commit-main" {
		t.Fatalf("the base's source is %q, want the default branch's archive", got)
	}
	if !strings.Contains(lima.input(tree), "TREE") {
		t.Fatalf("the base's runner tree is %q", lima.input(tree))
	}
	if !slices.Equal(mirror.fetched, []string{alphaRepo + "@main"}) {
		t.Fatalf("fetched %v, want the default branch alone", mirror.fetched)
	}
	if !strings.HasPrefix(img.Ref, vm.BaseName("alpha")+"@") {
		t.Fatalf("image %q", img.Ref)
	}

	if _, err := pr.ensureBase(context.Background(), p, m); err != nil || lima.count(create) != 1 {
		t.Fatalf("an unchanged key: %v, %d builds", err, lima.count(create))
	}
	key, _ := os.ReadFile(s.baseKeyPath("alpha"))

	mirror.setDigest("d2")
	lima.fail = "stop local-ci-docker-base-alpha-building"
	if _, err := pr.ensureBase(context.Background(), p, m); err == nil || !strings.HasPrefix(err.Error(), "docker base: ") {
		t.Fatalf("a failed build: %v", err)
	}
	if after, _ := os.ReadFile(s.baseKeyPath("alpha")); string(after) != string(key) {
		t.Fatal("a failed build replaced the stored key")
	}
	lima.fail = ""
	if _, err := pr.ensureBase(context.Background(), p, m); err != nil || lima.count(create) != 3 {
		t.Fatalf("a changed key: %v, %d builds, want 3", err, lima.count(create))
	}
}
