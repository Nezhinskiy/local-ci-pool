package runner

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

const testJIT = "eyJzZWNyZXQiOiJqaXQtY29uZmlnIn0="

func testSpec() ContainerSpec {
	return ContainerSpec{
		Image:    "local-ci/alpha:0123456789ab",
		User:     "1001",
		Name:     "local-ci-alpha-1",
		Instance: "main",
		Project:  "alpha",
		Mount:    mount.Mount{Type: mount.TypeImage, Source: "local-ci-pool/runner:2.338.0", Target: "/opt/local-ci", ReadOnly: true},
		JIT:      testJIT,
		CPUs:     2.5,
	}
}

func TestStartRunnerContract(t *testing.T) {
	d := newFakeDocker()
	s := testSpec()
	id, err := StartRunner(context.Background(), d, s)
	if err != nil {
		t.Fatal(err)
	}
	if id != "id-1" {
		t.Fatalf("id %q, want the created container's id", id)
	}
	if got := d.callLog(); !slices.Equal(got, []string{"network-inspect:local-ci-pool-main", "network-inspect:bridge", "network-create", "create", "attach", "start"}) {
		t.Fatalf("calls %v, want the network ensured, then create, attach, start", got)
	}
	if len(d.creates) != 1 {
		t.Fatalf("%d creates, want 1", len(d.creates))
	}
	o := d.creates[0]
	c, hc := o.Config, o.HostConfig
	if c == nil || hc == nil {
		t.Fatal("create without a config or a host config")
	}

	if o.Name != s.Name || c.Image != s.Image || c.User != s.User {
		t.Fatalf("name %q image %q user %q", o.Name, c.Image, c.User)
	}
	if !slices.Equal(c.Env, []string{"HOME=/tmp/home"}) {
		t.Fatalf("Env %q, want only HOME", c.Env)
	}
	if !slices.Equal(c.Entrypoint, []string{"/opt/local-ci/entrypoint.sh"}) {
		t.Fatalf("Entrypoint %q", c.Entrypoint)
	}
	if len(c.Cmd) != 0 {
		t.Fatalf("Cmd %q, want empty", c.Cmd)
	}
	for _, arg := range slices.Concat(c.Cmd, c.Entrypoint, c.Env) {
		if strings.Contains(arg, s.JIT) {
			t.Fatalf("the JIT configuration is in the command line or environment: %q", arg)
		}
	}
	if !c.OpenStdin || !c.StdinOnce || !c.AttachStdin || c.Tty {
		t.Fatalf("stdin: open %v once %v attach %v tty %v", c.OpenStdin, c.StdinOnce, c.AttachStdin, c.Tty)
	}

	if len(hc.Mounts) != 1 || hc.Mounts[0] != s.Mount || !hc.Mounts[0].ReadOnly {
		t.Fatalf("Mounts %+v, want exactly the read-only runner mount", hc.Mounts)
	}
	if len(hc.Binds) != 0 || len(hc.VolumesFrom) != 0 || len(c.Volumes) != 0 {
		t.Fatalf("Binds %q VolumesFrom %q Volumes %v, want none", hc.Binds, hc.VolumesFrom, c.Volumes)
	}
	if want := map[string]string{"/tmp/home": "exec"}; !maps.Equal(hc.Tmpfs, want) {
		t.Fatalf("Tmpfs %v, want %v", hc.Tmpfs, want)
	}
	if hc.Init == nil || !*hc.Init {
		t.Fatal("Init is not set")
	}
	if hc.ShmSize != 1<<30 || hc.Memory != 4<<30 || !hc.AutoRemove {
		t.Fatalf("ShmSize %d Memory %d AutoRemove %v", hc.ShmSize, hc.Memory, hc.AutoRemove)
	}
	if hc.Privileged || len(hc.CapAdd) != 0 || len(hc.Devices) != 0 || hc.NetworkMode.IsHost() || hc.PidMode.IsHost() {
		t.Fatalf("the container is not confined: %+v", hc)
	}
	// Every runner of the instance joins its own bridge, where containers
	// cannot reach each other (L1 of the security review).
	if hc.NetworkMode != "local-ci-pool-main" {
		t.Fatalf("NetworkMode %q, want the instance's network", hc.NetworkMode)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != 4096 {
		t.Fatalf("PidsLimit %v, want 4096", hc.PidsLimit)
	}
	if hc.NanoCPUs != 2_500_000_000 {
		t.Fatalf("NanoCPUs %d, want the spec's 2.5 CPUs", hc.NanoCPUs)
	}
	if want := []string{"host.docker.internal:127.0.0.1", "gateway.docker.internal:127.0.0.1"}; !slices.Equal(hc.ExtraHosts, want) {
		t.Fatalf("ExtraHosts %q, want %q", hc.ExtraHosts, want)
	}
	if want := map[string]string{"local-ci-pool": "main", "local-ci-pool.runner": s.Name, "local-ci-pool.project": "alpha"}; !maps.Equal(c.Labels, want) {
		t.Fatalf("Labels %v, want %v", c.Labels, want)
	}

	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "docker.sock") {
		t.Fatalf("the create call mentions docker.sock: %s", raw)
	}
	if strings.Contains(string(raw), s.JIT) {
		t.Fatalf("the JIT configuration is in the create call: %s", raw)
	}

	if got := d.stdinOf(t, id); got != s.JIT+"\n" {
		t.Fatalf("stdin %q, want the JIT and a newline", got)
	}
}

func TestStartRunnerLabels(t *testing.T) {
	for _, c := range []struct {
		name string
		need int
		want map[string]string
		env  []string
	}{
		{"with a need", 3, map[string]string{
			"local-ci-pool": "main", "local-ci-pool.runner": "local-ci-alpha-1",
			"local-ci-pool.project": "alpha", "local-ci-pool.need": "3",
		}, []string{"HOME=/tmp/home", "CORESLOT_CPU=3"}},
		{"without a need", 0, map[string]string{
			"local-ci-pool": "main", "local-ci-pool.runner": "local-ci-alpha-1",
			"local-ci-pool.project": "alpha",
		}, []string{"HOME=/tmp/home"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newFakeDocker()
			s := testSpec()
			s.Need = c.need
			if _, err := StartRunner(context.Background(), d, s); err != nil {
				t.Fatal(err)
			}
			if len(d.creates) != 1 || d.creates[0].Config == nil {
				t.Fatalf("%d creates, want 1 with a config", len(d.creates))
			}
			if got := d.creates[0].Config.Labels; !maps.Equal(got, c.want) {
				t.Fatalf("Labels %v, want %v", got, c.want)
			}
			// The job reads its grant to size its parallelism.
			if got := d.creates[0].Config.Env; !slices.Equal(got, c.env) {
				t.Fatalf("Env %q, want %q", got, c.env)
			}
		})
	}
}

func TestStartRunnerRemovesTheContainerWhenItCannotStart(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*fakeDocker)
		calls []string
	}{
		{"attach", func(d *fakeDocker) { d.attachErr = errors.New("attach refused") }, []string{"network-inspect:local-ci-pool-main", "network-inspect:bridge", "network-create", "create", "attach", "remove"}},
		{"start", func(d *fakeDocker) { d.startErr = errors.New("start refused") }, []string{"network-inspect:local-ci-pool-main", "network-inspect:bridge", "network-create", "create", "attach", "start", "remove"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newFakeDocker()
			c.setup(d)
			if _, err := StartRunner(context.Background(), d, testSpec()); err == nil {
				t.Fatal("StartRunner reported success")
			}
			if got := d.callLog(); !slices.Equal(got, c.calls) {
				t.Fatalf("calls %v, want %v", got, c.calls)
			}
			if got := d.removed(); !slices.Equal(got, []string{"id-1"}) {
				t.Fatalf("removed %v, want a forced remove of id-1", got)
			}
		})
	}
}

func TestStartRunnerCreatesTheNetworkOnce(t *testing.T) {
	d := newFakeDocker()
	for range 2 {
		if _, err := StartRunner(context.Background(), d, testSpec()); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.networkCreates) != 1 {
		t.Fatalf("%d network creates, want 1", len(d.networkCreates))
	}
	o := d.networkCreates[0]
	if o.Driver != "bridge" || o.Internal || o.Options["com.docker.network.bridge.enable_icc"] != "false" {
		t.Fatalf("network %+v, want a bridge with outbound traffic and no traffic between containers", o)
	}
	if o.Labels["local-ci-pool"] != "main" {
		t.Fatalf("network labels %v, want the instance", o.Labels)
	}
}

// The network takes the default bridge's MTU, where Docker puts a
// daemon-wide mtu; without one it sets none.
func TestStartRunnerNetworkTakesTheBridgeMTU(t *testing.T) {
	d := newFakeDocker()
	d.networks = map[string]network.Inspect{"bridge": {Network: network.Network{Driver: "bridge", Options: map[string]string{"com.docker.network.driver.mtu": "1400"}}}}
	if _, err := StartRunner(context.Background(), d, testSpec()); err != nil {
		t.Fatal(err)
	}
	if got := d.networkCreates[0].Options["com.docker.network.driver.mtu"]; got != "1400" {
		t.Fatalf("mtu %q, want the bridge's 1400", got)
	}
	d = newFakeDocker()
	if _, err := StartRunner(context.Background(), d, testSpec()); err != nil {
		t.Fatal(err)
	}
	if got, ok := d.networkCreates[0].Options["com.docker.network.driver.mtu"]; ok {
		t.Fatalf("mtu %q without a bridge to take it from", got)
	}
}

// Another process creating the network between the inspect and the create is
// not a failure: the network it made is inspected and checked like any.
func TestStartRunnerNetworkCreatedElsewhereMeanwhile(t *testing.T) {
	for name, c := range map[string]struct {
		made    network.Network
		wantErr bool
	}{
		"as the pool makes it": {network.Network{Driver: "bridge", Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"}}, false},
		"with icc on":          {network.Network{Driver: "bridge", Options: map[string]string{}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDocker()
			d.networkCreateHook = func(n string) error {
				d.networks = map[string]network.Inspect{n: {Network: c.made}}
				return cerrdefs.ErrConflict.WithMessage("network with name " + n + " already exists")
			}
			_, err := StartRunner(context.Background(), d, testSpec())
			if (err != nil) != c.wantErr {
				t.Fatalf("StartRunner: %v, want an error %v", err, c.wantErr)
			}
			if !c.wantErr && !slices.Contains(d.callLog(), "start") {
				t.Fatalf("calls %v, want the runner started", d.callLog())
			}
			if c.wantErr && slices.Contains(d.callLog(), "create") {
				t.Fatalf("calls %v, want no container", d.callLog())
			}
		})
	}
}

// A network of that name that lets containers talk to each other, or has no
// way out, is not used.
func TestStartRunnerRefusesAForeignNetwork(t *testing.T) {
	for name, n := range map[string]network.Network{
		"icc on":   {Driver: "bridge", Options: map[string]string{}},
		"internal": {Driver: "bridge", Internal: true, Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"}},
		"overlay":  {Driver: "overlay", Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDocker()
			d.networks = map[string]network.Inspect{"local-ci-pool-main": {Network: n}}
			_, err := StartRunner(context.Background(), d, testSpec())
			if err == nil || !strings.Contains(err.Error(), "local-ci-pool-main") {
				t.Fatalf("StartRunner on a %s network: %v", name, err)
			}
			if slices.Contains(d.callLog(), "create") {
				t.Fatalf("a container was created: %v", d.callLog())
			}
		})
	}
}

func TestStartRunnerNetworkInspectFailure(t *testing.T) {
	d := newFakeDocker()
	d.networkErr = errors.New("daemon gone")
	if _, err := StartRunner(context.Background(), d, testSpec()); err == nil || !strings.Contains(err.Error(), "daemon gone") {
		t.Fatalf("got %v", err)
	}
	if slices.Contains(d.callLog(), "create") {
		t.Fatalf("a container was created: %v", d.callLog())
	}
}

func TestStartRunnerWithoutACPUCap(t *testing.T) {
	d := newFakeDocker()
	s := testSpec()
	s.CPUs = 0
	if _, err := StartRunner(context.Background(), d, s); err != nil {
		t.Fatal(err)
	}
	if got := d.creates[0].HostConfig.NanoCPUs; got != 0 {
		t.Fatalf("NanoCPUs %d, want none", got)
	}
}

func TestCPUCap(t *testing.T) {
	for _, c := range []struct {
		need, cpus int
		want       float64
	}{
		{3, 8, 3},  // the budget's grant
		{12, 8, 8}, // a grant above the VM: Docker refuses more than it has
		{1, 8, 1},
		// Without a grant there is no hard cap: a lone job keeps the VM, and
		// busy jobs share it by the kernel's default weights.
		{0, 8, 0},
		{0, 10, 0},
		{3, 0, 0}, // nothing known about the VM
	} {
		if got := CPUCap(c.need, c.cpus); got != c.want {
			t.Errorf("CPUCap(%d, %d) = %v, want %v", c.need, c.cpus, got, c.want)
		}
	}
}

func TestStartRunnerRefusesABadSpec(t *testing.T) {
	for name, mutate := range map[string]func(*ContainerSpec){
		"no name":            func(s *ContainerSpec) { s.Name = "" },
		"no instance":        func(s *ContainerSpec) { s.Instance = "" },
		"no project":         func(s *ContainerSpec) { s.Project = "" },
		"no image":           func(s *ContainerSpec) { s.Image = "" },
		"no user":            func(s *ContainerSpec) { s.User = "" },
		"no jit":             func(s *ContainerSpec) { s.JIT = "" },
		"multi-line jit":     func(s *ContainerSpec) { s.JIT = "a\nb" },
		"writable mount":     func(s *ContainerSpec) { s.Mount.ReadOnly = false },
		"mount elsewhere":    func(s *ContainerSpec) { s.Mount.Target = "/opt" },
		"docker socket":      func(s *ContainerSpec) { s.Mount.Source = "/var/run/docker.sock" },
		"bind of a host dir": func(s *ContainerSpec) { s.Mount.Type = mount.TypeBind },
	} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDocker()
			s := testSpec()
			mutate(&s)
			if _, err := StartRunner(context.Background(), d, s); err == nil {
				t.Fatal("StartRunner accepted the spec")
			}
			if len(d.callLog()) != 0 {
				t.Fatalf("Docker was called: %v", d.callLog())
			}
		})
	}
}
