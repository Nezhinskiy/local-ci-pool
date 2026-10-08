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
  It runs as a non-root user with `--init` and a memory limit, and is removed after its job.
- A job container can reach your LAN through the Docker VM, and it can reach services on the Mac
  itself (see the residual below).
- Release binaries are built by this repository's CI and carry build provenance. Verify one with
  `gh attestation verify` before you run it.

## Residual risk: Mac loopback services are reachable from jobs

`ExtraHosts` points the two host names `host.docker.internal` and `gateway.docker.internal` at the
container's own loopback, so those names do not lead to the Mac. It does not close the Docker Desktop
host gateway address itself (`192.168.65.254` on current releases): a job that connects to that
address reaches every port bound to `127.0.0.1` on the Mac. That includes the pool's own `/healthz`,
which answers a forged loopback `Host` header and lists the repositories the pool serves, and any
other development service you run on loopback.

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
