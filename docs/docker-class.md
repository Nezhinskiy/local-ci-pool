# Design: the docker class (a throwaway VM per job)

Status: wired. The marker's `docker` block is parsed and validated (`internal/discovery`), the Lima backend
builds a base and runs a job in a clone of it (`internal/vm`), and the supervisor serves the class
(`internal/supervisor/dockerclass.go`). What the owner sees and does: [the runbook](runbook.md#the-docker-class).

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

## How a job runs

- The pool registers a second scale set for the repository, `<identity>-docker-<machine>`, labelled
  `<identity>-docker`, outside probe mode only, and only for a marker read at the default branch. It
  is a project of its own in `/healthz` (`"docker_class": true`), sharing the repository's visibility
  checks and drain. It writes no heartbeat: the routing action routes `<identity>-local` jobs alone, so
  a workflow names `<identity>-docker` in `runs-on` directly, and such a job waits for the Mac; it never
  falls back to a hosted runner.
- The pool keeps one base VM per repository (Lima, `vz`). It builds the base **only from the default branch**:
  - the pool's pinned Linux image, Docker Engine and the verified actions/runner tarball;
  - then `provision` from the default branch's archive, run as root (for example: install gVisor, pull and
    build the images the jobs will need);
  - then it stops the base.

  The base is rebuilt when its key changes. The key is a SHA-256 over the block's inputs digest at the
  default branch's commit and its `provision` path, Lima's template (`template:docker-rootful`, which pins
  the Linux image and Docker Engine) and `limactl --version`, the runner tree's image ID (the actions/runner
  release and the pool's entrypoint) and the VM's size, so a base never keeps an ageing runner. The key
  is stored in `~/Library/Application Support/local-ci-pool/docker-base-<identity>.key` once a build
  succeeds, and checked at start and on every discovery pass. A failed build keeps the old base and marks
  the class unhealthy (`docker base: …`), as a failed image build does, and is tried again on the next
  pass. One base builds at a time on the machine. A pull request's own `provision` never reaches the
  base, and probe mode does not build one.
- A job gets a copy-on-write clone of the stopped base:
  - booted with no host mounts, with guest-to-host port forwarding off, and with no Rosetta or containerd
    shares (Lima's plain mode);
  - its runner registered with a JIT config passed on stdin;
  - the clone deleted when the runner exits. At start-up the pool deletes every job clone a killed pool left
    behind.

  A job's writes, and its image builds and caches, stay in its clone; the base is never booted again.
- One docker VM runs at a time on a machine, whatever the repository (one machine-wide slot), with its own
  CPU and memory (4 CPUs, 6 GiB, a 30 GiB disk), outside Docker Desktop's slots and the core budget. A base
  build is a second VM and may run beside it. Below 15 GiB free where Lima keeps its VMs (`~/.lima`) the
  pool starts no VM: the runner it registered is unregistered again, and `/healthz` reports the class
  unhealthy (`docker vm: not enough free disk …`). The job stays queued; the pool never moves it elsewhere.
- A docker runner has no container: the Docker die events, the reconcile and the start-up sweep of
  containers never release or remove it. Its clone's end is its exit; an idle one beyond the assigned
  jobs, or one left at the drain bound, is removed by deleting its clone.

## What a job can reach (measured with Lima 2.3.0, `vz`, `--plain`)

- Root inside its own VM, and so the runner's own credentials. They are valid for that one runner and job,
  and die with the VM, as in a container job.
- The internet and the LAN through the VM's NAT, like a container job.
- **The Mac's loopback services**, as from a container job: Lima's user-mode gateway `192.168.5.2`
  (`host.lima.internal`) leads to the Mac's `127.0.0.1`. Listeners on `127.0.0.1` and `0.0.0.0` and a Docker
  Desktop port published on `127.0.0.1` all answered from the VM. No Lima setting closes the gateway while
  the VM stays usable (Lima's own SSH uses it), and firewall rules inside the VM do not count, because the
  job is root there ([SECURITY.md](../SECURITY.md)).
- Not the other way: in plain mode a port opened inside the VM is not forwarded to the Mac.

## Measured cost (Apple Silicon, 4 CPU / 6 GiB per VM)

| Step | Time |
|---|---|
| Base create (with the template image download) / first boot | 190 s / 126 s |
| Clone of the stopped base | 0.1 s, no disk at clone time (APFS copy-on-write) |
| Boot of a clone | 8 s |
| A Docker-and-gVisor test suite in a warm clone (image builds from the base's cache) | 72 s (13 s of builds) |
| Delete of a clone | 1 s |

The base takes 9.4 GiB; a job clone's writes, about 2–4 GiB, go with the clone.
