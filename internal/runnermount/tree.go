package runnermount

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// TreeDocker is the part of the Docker API Tree uses. *client.Client satisfies it.
type TreeDocker interface {
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerExport(ctx context.Context, containerID string, options client.ContainerExportOptions) (client.ContainerExportResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// initLayer is what Docker adds to every container's file system, a never-started one's included: none of it
// belongs to the tree.
var initLayer = map[string]bool{
	".dockerenv": true, ".dockerinit": true, "dev": true, "proc": true, "sys": true,
	"etc": true, "etc/hostname": true, "etc/hosts": true, "etc/resolv.conf": true, "etc/mtab": true,
}

// Tree streams the runner tree of m as a tar, for a docker-class base VM (internal/vm), which has no image
// mount: it exports a never-started container of the tree image, without the files Docker adds to every
// container. Close removes the container. A container a killed pool left is removed before the next export.
func Tree(ctx context.Context, d TreeDocker, m Mount) (io.ReadCloser, error) {
	name := "local-ci-tree-" + m.Version
	remove := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = d.ContainerRemove(rctx, name, client.ContainerRemoveOptions{Force: true})
	}
	remove()
	if _, err := d.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		// A FROM scratch image has no command, and create wants one; the container never starts.
		Config:     &container.Config{Image: m.Spec.Source, Entrypoint: []string{Target + "/entrypoint.sh"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	}); err != nil {
		return nil, err
	}
	exp, err := d.ContainerExport(ctx, name, client.ContainerExportOptions{})
	if err != nil {
		remove()
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(filterInit(pw, exp)) }()
	return &tree{PipeReader: pr, exp: exp, remove: remove}, nil
}

type tree struct {
	*io.PipeReader
	exp    io.Closer
	remove func()
}

func (t *tree) Close() error {
	_ = t.PipeReader.Close()
	err := t.exp.Close()
	t.remove()
	return err
}

// filterInit copies the tar from r to w without the init layer's entries.
func filterInit(w io.Writer, r io.Reader) error {
	tr, tw := tar.NewReader(r), tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean(hdr.Name), "/")
		top, _, _ := strings.Cut(name, "/")
		if initLayer[name] || (top != "etc" && initLayer[top]) {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

var _ TreeDocker = (*client.Client)(nil)
