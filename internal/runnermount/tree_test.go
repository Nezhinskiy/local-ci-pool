package runnermount

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

type fakeExporter struct {
	export  []byte
	created []client.ContainerCreateOptions
	removed []string
}

func (f *fakeExporter) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.created = append(f.created, o)
	return client.ContainerCreateResult{ID: o.Name}, nil
}

func (f *fakeExporter) ContainerExport(context.Context, string, client.ContainerExportOptions) (client.ContainerExportResult, error) {
	return io.NopCloser(bytes.NewReader(f.export)), nil
}

func (f *fakeExporter) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removed = append(f.removed, id)
	return client.ContainerRemoveResult{}, errors.New("no such container") // removing what is not there is fine
}

func tarNames(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		hdr := &tar.Header{Name: n, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(n))}
		if strings.HasSuffix(n, "/") {
			hdr.Typeflag, hdr.Size, hdr.Mode = tar.TypeDir, 0, 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			_, _ = tw.Write([]byte(n))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The tree a base VM gets is the tree image's files alone: what Docker adds to every container is left out,
// and the export container is removed, before the export (one a killed pool left) and at Close.
func TestTreeIsTheImageWithoutTheInitLayer(t *testing.T) {
	f := &fakeExporter{export: tarNames(t,
		".dockerenv", "bin/", "bin/Runner.Listener", "dev/", "dev/console", "dev/pts/", "entrypoint.sh",
		"etc/", "etc/hostname", "etc/hosts", "etc/mtab", "etc/resolv.conf", "proc/", "run.sh", "sys/")}
	m := Mount{Version: "2.338.0", Spec: mount.Mount{Type: mount.TypeImage, Source: Ref("2.338.0")}}
	rc, err := Tree(context.Background(), f, m)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if hdr.Typeflag == tar.TypeReg && string(body) != hdr.Name {
			t.Fatalf("%s carries %q", hdr.Name, body)
		}
		got = append(got, hdr.Name)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"bin/", "bin/Runner.Listener", "entrypoint.sh", "run.sh"}; !slices.Equal(got, want) {
		t.Fatalf("tree %v, want %v", got, want)
	}
	if len(f.created) != 1 || f.created[0].Config.Image != Ref("2.338.0") || f.created[0].Name != "local-ci-tree-2.338.0" {
		t.Fatalf("created %+v", f.created)
	}
	if want := []string{"local-ci-tree-2.338.0", "local-ci-tree-2.338.0"}; !slices.Equal(f.removed, want) {
		t.Fatalf("removed %v, want %v", f.removed, want)
	}
}
