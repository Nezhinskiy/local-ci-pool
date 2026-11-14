# Design: the docker class (a throwaway VM per job)

Status: implemented, the marker's `docker` block, parsed and validated (`internal/discovery`), and the Lima
backend that builds a base and runs a job in a clone of it (`internal/vm`). The pool does not wire the backend
in yet; everything below marked planned is the design.

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
  - Lima's `docker-rootful` template (Ubuntu, with Docker Engine from Docker's install script, so the Engine
    version is not pinned), and the verified actions/runner tarball;
  - then `provision` from the default branch's archive, run as root (for example: install gVisor, pull and
    build the images the jobs will need);
  - then it stops the base.

  The base's key is the block's inputs digest on the default branch, the `limactl` version, the
  actions/runner release, and the Docker Engine version read back from the built base and recorded. The base
  is rebuilt when the key changes. A new runner release already forces rebuilds every few weeks, which also
  picks up a newer Engine and Ubuntu, so there is no scheduled rebuild. A failed build keeps the old base and marks the class
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

## What a job can reach (measured with Lima 2.3.0, `vz`, `--plain`)

- Root inside its own VM, and so the runner's own credentials. They are valid for that one runner and job,
  and die with the VM, as in a container job.
- The internet and the LAN through the VM's NAT, like a container job.
- **The Mac's loopback services**, as from a container job: Lima's user-mode gateway `192.168.5.2`
  (`host.lima.internal`) leads to the Mac's `127.0.0.1`. Listeners on `127.0.0.1` and `0.0.0.0` and a Docker
  Desktop port published on `127.0.0.1` all answered from the VM. Firewall rules inside the VM do not
  count, because the job is root there ([SECURITY.md](../SECURITY.md)).
- Lima itself reaches into the VM: it forwards a port on the Mac's loopback to the VM's SSH, which the pool
  uses to run the runner. A local process on the Mac that holds Lima's key could open a shell in a job VM.
- Not the other way: in plain mode a port opened inside the VM is not forwarded to the Mac.

## Measured cost (Apple Silicon, 4 CPU / 6 GiB per VM)

| Step | Time |
|---|---|
| Base create (with the template image download) / first boot | 190 s / 126 s |
| Clone of the stopped base | 0.1 s, no disk at clone time (APFS copy-on-write) |
| Boot of a clone | 8 s |
| A Docker-and-gVisor test suite in a warm clone (image builds from the base's cache) | 72 s (13 s of builds) |
| Delete of a clone | 1 s |

The base takes 9.4 GiB; a job clone's writes (an estimate: about 2–4 GiB) go with the clone.
