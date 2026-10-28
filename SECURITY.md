# Security

## Threat model

The pool serves **private repositories that you own and trust**. Everything that runs in a job
container is code from those repositories. It is not a sandbox for untrusted code:

- Do not point it at a repository that accepts workflow runs from forks or from people you do not
  trust. Public repositories are refused.
- Repository owner, pool operator and the account behind the GitHub login are the same person.

## Blast radius

- The pool process uses your `gh` login at run time. A compromised pool process holds that
  login's rights over every repository the account owns. The token is read from `gh auth token`,
  never written to disk, never logged and never passed to a container.
- Jobs never see that token. A job sees the `GITHUB_TOKEN` its workflow grants it. Jobs that need
  `contents: write` should stay on hosted runners, so that no token that can move a default branch
  reaches the Mac.
- A job container mounts only the runner image mount, read-only: no host volumes and no Docker socket.
  It runs with `--init`, a 4 GiB memory limit and a limit of 4096 processes and threads, and is
  removed after its job. With the core budget on, it is also capped at its grant of CPUs; with the
  budget off, it has no CPU cap, and busy jobs share the VM's CPUs by the kernel's equal default
  weights. The consequence is that nothing inside a job can use Docker (`services:`, a job `container:`,
  Docker container actions, `docker build`); see [What does not work](README.md#what-does-not-work).
- A job runs as a non-root user, outside group root. The image's `USER` is kept unless it is root,
  and root is refused in every spelling: `root`, any number Docker reads as 0 (`0`, `00`, `+0`,
  `0000:0`), and a passwd name with uid 0, which the preflight catches by reading the effective uid
  from the kernel. Such an image runs as `1001:1001` instead. A group of root (`1000:0`, or a passwd
  entry whose primary group is 0) becomes group 1001 with the same user. The preflight empties
  `BASH_ENV` and `ENV`, so an image's start-up file cannot print a uid line before the entrypoint's.
  The check still guards against an image that is root by mistake, not one built to be: the uid is
  reported from inside the image, so such an image can fake it (with its own `bash`, say), and a
  setuid program in the image, such as a passwordless `sudo`, lets a job step become root inside
  its container anyway. The pool does not set `no-new-privileges`, which would stop that, because
  it would also break every image whose jobs rely on `sudo`. Keep such programs out of the images
  you serve.
- Job containers join one bridge network per pool instance, shared by every project the instance
  serves: `local-ci-pool-main` for the installed pool, `local-ci-pool-probe-<owner>` for a probe.
  It is not one network per project. Traffic between the containers on it is off
  (`com.docker.network.bridge.enable_icc=false`), so a job cannot reach a port that another job
  opened, of its own repository or another, and cannot reach containers on Docker's default bridge
  either. The network takes the default bridge's MTU, where Docker puts a daemon-wide `mtu`. It can still reach the internet, your LAN through the Docker VM, and
  services on the Mac itself (see the residual below).
- Release binaries are built by this repository's CI and carry build provenance. Verify one with
  `gh attestation verify` before you run it.

## The routing and the heartbeat variables

The `route` action decides from repository variables, `CI_POOL_HB_<MACHINE>`, and anyone with write
access to the repository can set a variable. That person can already edit the workflows, so running
code gives them nothing they lack. What the variable gives them is routing:

- Availability. A fresh heartbeat sends jobs to the local label. If no runner serves that label, for
  example because the heartbeat was forged, the jobs sit queued (GitHub keeps a queued job up to 24
  hours) instead of falling back to the hosted label.
- With the `local-label` input, a different target. A heartbeat routes jobs to whatever self-hosted
  runner serves that label, which may be another machine than the one you meant.

`route/heartbeat.sh` writes a heartbeat for runners that are not this pool. Run it with a
fine-grained personal access token limited to that one repository, with *Variables: read and
write*, passed as `GH_TOKEN`, and not with the ambient `gh` login, which is usually a broad
`repo`-scoped token on a machine where the script runs unattended. Keep the file that holds the token
private (`chmod 600`). `heartbeat.sh` refuses a public repository (exit 2), as the pool does, because a
pull request from a fork would run on your machine. The check is the repository's visibility, not
its owner, so a private repository owned by an organization is served.

## Residual risk: Mac loopback services are reachable from jobs

`ExtraHosts` points the two host names `host.docker.internal` and `gateway.docker.internal` at the
container's own loopback, so those names do not lead to the Mac. It does not close the Docker Desktop
host gateway address itself (`192.168.65.254` on current releases): a job that connects to that
address reaches every port bound to `127.0.0.1` on the Mac. That includes the pool's own `/healthz`,
which answers a forged loopback `Host` header, and any other development service you run on
loopback.

What a job reads from `/healthz` is the redacted body: the pool's state, the repositories it serves,
whether each is healthy, and a fixed class for why one is not (`setup`, `image`, `preflight`, …). The
reasons themselves can quote another project's build or preflight output, so they, the image
references and the names of stuck jobs go only to a request that carries the pool's health token.
The pool writes a new random token at each start to
`~/Library/Application Support/local-ci-pool/health-<port>.token` (mode 0600), which `pool status`
reads and no job can.

With the core budget, `/healthz` also shows a top-level `budget` object (`state`, an `error` class,
`oldest_wait_s`) and, per project, `waiting` and `admitted` counts. These carry only the pool's own
data: its budget state, an error class (`config`, `open` or `state`), its oldest wait in seconds and
its per-project counts. They never name another workload's label, a path or a library message.

Mitigation is owner discipline: run on the Mac only loopback services you would let your own
repositories' jobs use, and give the ones that matter their own authentication. Real egress
filtering would need root inside the job container (or a firewall in the Docker VM) and is out of
scope for this pool.

## Residual risk: no disk cap

A job can fill the Docker VM's disk by writing to its container's filesystem. Concurrent jobs of
every project, the pool's image builds and Docker itself then fail until the job ends and its
container is removed. There is no per-container cap on Docker Desktop: with its containerd image
store, `--storage-opt size=` is accepted and ignored (measured on Docker Desktop with Engine 29.9: a
300 MB write passed a 100 MB cap). Size the VM disk under Docker Desktop > Settings > Resources with
room for the largest job, and `docker system df` shows what takes it.

## Residual risk: the JIT configuration on the same UID

The just-in-time runner configuration reaches the runner as a command-line argument, and the runner
decodes it into credential files (`.runner`, `.credentials` and the key file) under its runner root,
`/tmp/runner`, which is writable by the job's user. A job step running as that same user can read
the argv of the runner process and those files. The configuration is single-use and the runner is
ephemeral, which bounds the impact: it cannot be replayed once the job ends.

## Reporting

Report a vulnerability through GitHub private vulnerability reporting on this repository (the
Security tab, "Report a vulnerability"). Do not open a public issue for a security report.
