//go:build docker

package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
)

// A bare Ubuntu: bash, no CA bundle, no libicu. The runner never gets as far
// as the network here, which is all this test needs.
const bareUbuntu = "ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55"

// TestRealRunnerExitReachesWatchExits starts a runner container with a JIT
// configuration the runner cannot use, against the real daemon and the real
// runner tree, and checks that its exit comes back through WatchExits under its
// runner name although the container removes itself, and that the entrypoint
// got the configuration from stdin (it exits 2 when stdin is empty).
func TestRealRunnerExitReachesWatchExits(t *testing.T) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ref := runnermount.Ref("2.338.0")
	if _, err := cli.ImageInspect(ctx, ref); err != nil {
		t.Skipf("runner tree %s is not built here: %v", ref, err)
	}
	if _, err := cli.ImageInspect(ctx, bareUbuntu); err != nil {
		t.Skipf("%s is not pulled here: %v", bareUbuntu, err)
	}

	instance := "probe-test-" + time.Now().Format("150405.000")
	name := "local-ci-probe-" + time.Now().Format("150405000")

	// The test's own subscription reads the exit code; WatchExits only maps
	// names.
	codes := make(chan string, 1)
	sub := cli.Events(ctx, client.EventsListOptions{Filters: make(client.Filters).
		Add("type", "container").Add("event", "die").Add("label", runner.LabelInstance+"="+instance)})
	go func() {
		for {
			select {
			case m := <-sub.Messages:
				if m.Action == events.ActionDie {
					codes <- m.Actor.Attributes["exitCode"]
					return
				}
			case <-sub.Err:
				return
			}
		}
	}()

	exits := make(chan string, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go runner.WatchExits(watchCtx, cli, instance, func(n string) { exits <- n })

	id, err := runner.StartRunner(ctx, cli, runner.ContainerSpec{
		Image:    bareUbuntu,
		User:     "1001",
		Name:     name,
		Instance: instance,
		Mount:    mount.Mount{Type: mount.TypeImage, Source: ref, Target: runnermount.Target, ReadOnly: true},
		JIT:      "not-a-jit-configuration",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = runner.RemoveContainer(context.WithoutCancel(ctx), cli, id)
	}()

	select {
	case got := <-exits:
		if got != name {
			t.Fatalf("WatchExits mapped the exit to %q, want %q", got, name)
		}
	case <-ctx.Done():
		t.Fatal("no exit reached WatchExits")
	}
	select {
	case code := <-codes:
		t.Logf("runner container exited %s", code)
		if code == "2" || code == "" {
			t.Fatalf("exit code %q: the entrypoint did not get the JIT configuration on stdin", code)
		}
	case <-ctx.Done():
		t.Fatal("no die event with an exit code")
	}
	// AutoRemove: the container is gone, and removing it again is not an error.
	if err := runner.RemoveContainer(ctx, cli, name); err != nil {
		t.Fatalf("removing the auto-removed container: %v", err)
	}
}
