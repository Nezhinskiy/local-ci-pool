//go:build docker

package image_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Nezhinskiy/local-ci-pool/internal/dockerutil"
	"github.com/Nezhinskiy/local-ci-pool/internal/image"
	"github.com/Nezhinskiy/local-ci-pool/internal/runnermount"
	"github.com/Nezhinskiy/local-ci-pool/runner"
)

// A bare Ubuntu: bash, coreutils, a passwd with root and ubuntu (uid 1000).
const bareUbuntu = "ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55"

// build builds an image tagged tag from files, and removes it when the test
// ends.
func build(t *testing.T, ctx context.Context, cli *client.Client, tag string, files map[string]string, modes map[string]int64) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		mode := int64(0o644)
		if m, ok := modes[name]; ok {
			mode = m
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := cli.ImageBuild(ctx, &buf, client.ImageBuildOptions{Tags: []string{tag}, Remove: true, ForceRemove: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := dockerutil.DrainStream(resp.Body); err != nil {
		t.Fatalf("building %s: %v", tag, err)
	}
	t.Cleanup(func() {
		_, _ = cli.ImageRemove(context.Background(), tag, client.ImageRemoveOptions{Force: true})
	})
}

// TestRealPreflightNeverRunsAJobAsRoot checks against the real daemon that an
// image whose USER is root in any spelling Docker accepts gets DefaultUser (L2
// of the security review), and that a non-root user is kept. The runner tree is
// a stand-in built from the real entrypoint and a Runner.Listener that prints
// a version, so the test needs no runner download; the job images set the
// entrypoint's test hooks only to accept /etc/passwd as their CA bundle.
func TestRealPreflightNeverRunsAJobAsRoot(t *testing.T) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := cli.ImageInspect(ctx, bareUbuntu); err != nil {
		t.Skipf("%s is not pulled here: %v", bareUbuntu, err)
	}
	stamp := time.Now().Format("150405000")

	tree := "local-ci-pool-test/runner:0.0.0-uid-" + stamp
	build(t, ctx, cli, tree, map[string]string{
		"Dockerfile":          "FROM scratch\nCOPY . /\n",
		"entrypoint.sh":       string(runner.Entrypoint),
		"bin/Runner.Listener": "#!/bin/sh\necho 2.338.0\n",
	}, map[string]int64{"entrypoint.sh": 0o755, "bin/Runner.Listener": 0o755})
	m := runnermount.Mount{Version: "2.338.0", Spec: mount.Mount{Type: mount.TypeImage, Source: tree, Target: runnermount.Target, ReadOnly: true}}

	const hooks = "FROM " + bareUbuntu + "\nENV LOCAL_CI_TEST_HOOKS=1 LOCAL_CI_CA_PATHS=/etc/passwd\n"
	for _, c := range []struct {
		name, dockerfile, want string
	}{
		{"double zero", hooks + "USER 00\n", image.DefaultUser},
		{"zeros with a group", hooks + "USER 0000:0\n", image.DefaultUser},
		{"plus zero", hooks + "USER +0\n", image.DefaultUser},
		{"a passwd name with uid 0", hooks + "RUN echo 'toor:x:0:0:root:/root:/bin/bash' >> /etc/passwd\nUSER toor\n", image.DefaultUser},
		{"no user", hooks, image.DefaultUser},
		// The image's BASH_ENV would run first and print a forged uid line.
		// Its uid line comes first, and the first one is the one read.
		{"a forged uid line", hooks + "RUN echo 'toor:x:0:0:root:/root:/bin/bash' >> /etc/passwd && echo 'echo \"local-ci: uid 1000 gid 1000\"' > /forge.sh\nENV BASH_ENV=/forge.sh ENV=/forge.sh\nUSER toor\n", image.DefaultUser},
		{"a group of 0", hooks + "USER 1000:0\n", "1000:1001"},
		{"a passwd entry in group root", hooks + "RUN useradd -u 1500 -g 0 -M rootgroup\nUSER rootgroup\n", "rootgroup:1001"},
		{"a member of group root", hooks + "RUN useradd -u 1500 -M member && sed -i 's/^root:x:0:$/root:x:0:member/' /etc/group\nUSER member\n", "member:1001"},
		{"a non-root name", hooks + "USER ubuntu\n", "ubuntu"},
		{"a non-root uid", hooks + "USER 1000\n", "1000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tag := "local-ci-pool-test/job:uid-" + stamp
			build(t, ctx, cli, tag, map[string]string{"Dockerfile": c.dockerfile}, nil)
			user, version, err := image.PreflightVersion(ctx, cli, tag, m)
			if err != nil {
				t.Fatal(err)
			}
			if user != c.want || version != "2.338.0" {
				t.Fatalf("user %q, version %q; want %q, 2.338.0", user, version, c.want)
			}
		})
	}
}
