//go:build docker

package runner_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/runner"
)

// A small public image with Python, for a server and its clients.
const pythonAlpine = "python:3.12-alpine@sha256:1b668429b3511ab407d8e00648891631b0b1a4d7e15e3ca70f38ab5b91ad4ab4"

// TestRealNetworkKeepsJobsApart checks, against the real daemon, the property
// the runners' network exists for (L1 of the security review): a job cannot
// reach a port another job on the network has open, nor a container on
// Docker's default bridge, and it still reaches the internet. A server and a
// client on the default bridge show that the probe itself works. Every
// container and the network are the test's own, under a test instance.
func TestRealNetworkKeepsJobsApart(t *testing.T) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := cli.ImageInspect(ctx, pythonAlpine); err != nil {
		t.Skipf("%s is not pulled here: %v", pythonAlpine, err)
	}

	instance := "probe-test-" + time.Now().Format("150405000")
	if err := runner.EnsureNetwork(ctx, cli, instance); err != nil {
		t.Fatal(err)
	}
	net := runner.NetworkName(instance)
	var started []string
	defer func() {
		bg := context.WithoutCancel(ctx)
		for _, id := range started {
			_, _ = cli.ContainerRemove(bg, id, client.ContainerRemoveOptions{Force: true})
		}
		if _, err := cli.NetworkRemove(bg, net, client.NetworkRemoveOptions{}); err != nil {
			t.Errorf("removing the test network: %v", err)
		}
	}()
	// Ensuring it again finds it and keeps it.
	if err := runner.EnsureNetwork(ctx, cli, instance); err != nil {
		t.Fatalf("a second EnsureNetwork: %v", err)
	}

	run := func(network string, cmd ...string) string {
		t.Helper()
		created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:     &container.Config{Image: pythonAlpine, User: "1001", Cmd: cmd, Labels: map[string]string{runner.LabelInstance: instance}},
			HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(network)},
		})
		if err != nil {
			t.Fatal(err)
		}
		started = append(started, created.ID)
		if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	ipOf := func(id, network string) string {
		t.Helper()
		got, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ep := got.Container.NetworkSettings.Networks[network]
		if ep == nil || !ep.IPAddress.IsValid() {
			t.Fatalf("container %s has no address on %s", id, network)
		}
		return ep.IPAddress.String()
	}
	// exitOf runs a client to the end and returns its exit code.
	exitOf := func(network, script string) int64 {
		t.Helper()
		id := run(network, "python3", "-c", script)
		w := cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
		select {
		case res := <-w.Result:
			return res.StatusCode
		case err := <-w.Error:
			t.Fatal(err)
		}
		return -1
	}
	connect := func(addr string) string {
		return fmt.Sprintf("import socket; socket.create_connection((%q, 8080), timeout=3)", addr)
	}

	jobServer := run(net, "python3", "-m", "http.server", "8080")
	bridgeServer := run("bridge", "python3", "-m", "http.server", "8080")
	jobIP, bridgeIP := ipOf(jobServer, net), ipOf(bridgeServer, "bridge")
	time.Sleep(2 * time.Second) // the servers bind

	if code := exitOf("bridge", connect(bridgeIP)); code != 0 {
		t.Fatalf("the probe does not work: a client on the default bridge could not reach a server there (exit %d)", code)
	}
	if code := exitOf(net, connect(jobIP)); code == 0 {
		t.Errorf("a job reached another job's port at %s:8080 on %s", jobIP, net)
	}
	if code := exitOf(net, connect(bridgeIP)); code == 0 {
		t.Errorf("a job reached a container on the default bridge at %s:8080", bridgeIP)
	}

	// Outbound traffic, where the machine has it at all.
	out := `import socket; socket.create_connection(("1.1.1.1", 443), timeout=5)`
	if exitOf("bridge", out) != 0 {
		t.Log("skipping the outbound check: the default bridge has no way out here either")
		return
	}
	if code := exitOf(net, out); code != 0 {
		t.Errorf("a job on %s cannot reach the internet (exit %d)", net, code)
	}
}
