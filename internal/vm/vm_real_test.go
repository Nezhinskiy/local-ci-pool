//go:build lima

package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The smoke test against the real limactl: it builds a base with a provision that leaves a mark, launches a
// job whose entrypoint checks its own VM from inside, and sweeps. About five minutes; it needs Lima and the
// network (the template's image). go test -tags lima ./internal/vm/
func TestRealBaseAndJob(t *testing.T) {
	home, _ := os.UserHomeDir()
	b := &Backend{Lima: Exec{}, Instance: "smoke", CPUs: 2, MemoryGiB: 2, DiskGiB: 20, MinFreeGiB: 10, FreeGiB: FreeGiBAt(filepath.Join(home, ".lima"))}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	identity := "smoke-" + time.Now().Format("150405")
	baseName, err := b.BaseName(identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = b.Lima.Run(context.Background(), nil, "unprotect", baseName)
		_, _ = b.Lima.Run(context.Background(), nil, "delete", "--force", baseName)
	})

	// The entrypoint checks, from inside the job, what docs/docker-class.md promises, and fails otherwise.
	entrypoint := `#!/bin/sh
set -eu
IFS= read -r jit
[ "$jit" = JIT-SMOKE ] || { echo "jit: $jit" >&2; exit 11; }
[ "$(id -un)" = runner ] || exit 12
id -nG | grep -qw docker || exit 13
docker info >/dev/null || exit 14
[ -f /etc/local-ci-provisioned ] || exit 15
[ ! -e /opt/local-ci-source ] || exit 16
! mount | grep -qE ' type (virtiofs|9p|fuse.sshfs) ' || exit 17
exit 0
`
	// A symlinked provision is refused inside the VM: the build fails and leaves no base.
	if err := b.BuildBase(ctx, identity, tarOf(t, map[string]string{"entrypoint.sh": entrypoint}),
		tarWithLink(t, "ci/vm/provision.sh", "/etc/hostname"), "ci/vm/provision.sh"); err == nil {
		t.Fatal("a symlinked provision built a base")
	}
	if err := b.BuildBase(ctx, identity, tarOf(t, map[string]string{"entrypoint.sh": entrypoint}),
		tarWithLink(t, "ci", "/etc"), "ci/hostname"); err == nil {
		t.Fatal("a provision under a symlinked directory built a base")
	}
	if err := b.BuildBase(ctx, identity, tarOf(t, map[string]string{"entrypoint.sh": entrypoint}),
		tarOf(t, map[string]string{"ci/vm/provision.sh": "touch /etc/local-ci-provisioned\n"}), "ci/vm/provision.sh"); err != nil {
		t.Fatal(err)
	}
	// A second build replaces the first through the swap; the first base is gone afterwards.
	if err := b.BuildBase(ctx, identity, tarOf(t, map[string]string{"entrypoint.sh": entrypoint}),
		tarOf(t, map[string]string{"ci/vm/provision.sh": "touch /etc/local-ci-provisioned\n"}), "ci/vm/provision.sh"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	done := make(chan error, 1)
	if err := b.Launch(ctx, identity, "local-ci-smoke", "JIT-SMOKE", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the job's checks failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the job did not finish")
	}
	out, err := b.Lima.Run(ctx, nil, "list", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	jobName, _ := b.JobName("local-ci-smoke")
	if strings.Contains(string(out), jobName) {
		t.Fatal("the job clone survived its runner")
	}
}

func tarOf(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func tarWithLink(t *testing.T, name, target string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}
