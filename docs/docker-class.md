# Design: the docker class (a throwaway VM per job)

Status: implemented, the marker's `docker` block, parsed and validated (`internal/discovery`). The pool does
not act on it yet; everything below marked planned is the design.

A pool job runs in a container on Docker Desktop's VM, as a non-root user, with no Docker socket
([SECURITY.md](../SECURITY.md)). A job that needs a Docker daemon of its own, for example to build images or to
run containers under gVisor, cannot run there. Mounting the socket would hand that job root over every other
job and runner on the Mac. The docker class gives such a job a Linux VM of its own instead, so root inside it
reaches nothing else.

## Opt-in

The marker gains an optional block next to either form:

```json
{
  "dockerfile": "ci/runner/Dockerfile",
  "inputs": ["ci/runner"],
  "docker": { "provision": "ci/docker-vm/provision.sh", "inputs": ["ci/docker-vm", "sandbox"] }
}
```

`provision` is a script inside `inputs`, and every path follows the same rules as the image inputs. A pool
that predates the block refuses a marker carrying it (`DisallowUnknownFields`): for a repository it already
serves, that drains the repository, its `<identity>-local` jobs included. **Upgrade the pool on every Mac
before a repository commits the block.**

## How a job runs (planned)

- The pool registers a second scale set for the repository, labelled `<identity>-docker`.
- The pool keeps one base VM per repository (Lima, `vz`). It builds the base **only from the default branch**:
  - the pool's pinned Linux image, Docker Engine and the verified actions/runner tarball;
  - then `provision` from the default branch's archive, run as root (for example: install gVisor, pull and
    build the images the jobs will need);
  - then it stops the base.

  The base is rebuilt when the block's inputs change on the default branch, or when the pool's pinned
  Linux image, Docker Engine or actions/runner release changes: those four make the base's key, so a base
  never keeps an ageing runner or an unpatched Engine. A failed build keeps the old base and marks the class
  unhealthy, as a failed image build does. A pull request's own `provision` never reaches the base, and
  probe mode does not build one.
- A job gets a copy-on-write clone of the stopped base:
  - booted with no host mounts, with guest-to-host port forwarding off, and with no Rosetta or containerd
    shares (Lima's plain mode);
  - its runner registered with a JIT config passed on stdin;
  - the clone deleted when the runner exits. At start-up the pool deletes every job clone a killed pool left
    behind.

  A job's writes, and its image builds and caches, stay in its clone; the base is never booted again.
- One docker VM runs at a time on a machine, whatever the repository, with its own CPU and memory, outside
  Docker Desktop's slots. Below a free-disk threshold the pool starts no VM, and `/healthz` reports the
  class unhealthy with the reason. The job stays queued; the pool never moves it elsewhere.

## What a job can reach (planned, to be measured)

- Root inside its own VM, and so the runner's own credentials. They are valid for that one runner and job,
  and die with the VM, as in a container job.
- The internet and the LAN through the VM's NAT, like a container job.
- The Mac's loopback services: to be measured before the class ships, and stated here and in SECURITY.md.
  Firewall rules inside the VM do not count, because the job is root there.
