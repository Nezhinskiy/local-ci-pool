# Security

## Threat model

The pool serves **private repositories that you own and trust**. Everything that runs in a job
container is code from those repositories. It is not a sandbox for untrusted code:

- Do not point it at a repository that accepts workflow runs from forks or from people you do not
  trust. Public repositories are refused. A served repository that turns public can still get a
  runner for at most one minute: before each runner starts, the pool needs GitHub's answer that the
  repository is private, and it caches that answer for one minute. While the project has jobs
  assigned, the pool also asks again at its listener's next call (about every 50 seconds) once
  the answer is a minute old. The age is read by wall clock, so a sleep of the Mac does not keep an
  old answer fresh. When the answer is "not private", it refuses the runner, stops and removes the
  project's job containers at once, busy ones and those a previous pool process left included, and
  deletes the scale set, retrying for up to three minutes while GitHub still counts a removed job.
  A failed check, or an answer that names no visibility, refuses the runner too; it is never read
  as private.
- Repository owner, pool operator and the account behind the GitHub login are the same person.

## Blast radius

- The pool process uses your `gh` login at run time. A compromised pool process holds that
  login's rights over every repository the account owns. The token is read from `gh auth token`,
  never written to disk, never logged and never passed to a container.
- Jobs never see that token. A job sees the `GITHUB_TOKEN` its workflow grants it. Jobs that need
  `contents: write` should stay on hosted runners, so that no token that can move a default branch
  reaches the Mac.
- A job container mounts only the runner image mount, read-only: no host volumes and no Docker socket.
  It runs as a non-root user with `--init` and a memory limit, and is removed after its job. The
  consequence is that nothing inside a job can use Docker (`services:`, a job `container:`, Docker
  container actions, `docker build`); see [What does not work](README.md#what-does-not-work).
- A job container can reach your LAN through the Docker VM, and it can reach services on the Mac
  itself (see the residual below).
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
which answers a forged loopback `Host` header and lists the repositories the pool serves, and any
other development service you run on loopback.

With the core budget, `/healthz` also shows a top-level `budget` object (`state`, an `error` class,
`oldest_wait_s`) and, per project, `waiting` and `admitted` counts. These carry only the pool's own
data: its budget state, an error class (`config`, `open` or `state`), its oldest wait in seconds and
its per-project counts. They never name another workload's label, a path or a library message.

Mitigation is owner discipline: run on the Mac only loopback services you would let your own
repositories' jobs use, and give the ones that matter their own authentication. Real egress
filtering would need root inside the job container (or a firewall in the Docker VM) and is out of
scope for this pool.

## Residual risk: the JIT configuration on the same UID

The just-in-time runner configuration reaches the runner as a command-line argument, and the runner
decodes it into credential files (`.runner`, `.credentials` and the key file) under its runner root,
`/tmp/runner`, which is writable by the job's user. A job step running as that same user can read
the argv of the runner process and those files. The configuration is single-use and the runner is
ephemeral, which bounds the impact: it cannot be replayed once the job ends.

## Reporting

Report a vulnerability through GitHub private vulnerability reporting on this repository (the
Security tab, "Report a vulnerability"). Do not open a public issue for a security report.
