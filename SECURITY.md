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
- A job container mounts only the runner volume, read-only: no host volumes and no Docker socket.
  It runs as a non-root user with `--init` and a memory limit, and is removed after its job.
- A job container can reach your LAN through the Docker VM. Host loopback services are closed by
  `ExtraHosts` entries for `host.docker.internal` and `gateway.docker.internal`, and by publishing
  the pool's own ports on loopback only.
- Release binaries are built by this repository's CI and carry build provenance. Verify one with
  `gh attestation verify` before you run it.

## Residual risk: argv on the same UID

The just-in-time runner configuration reaches the runner as a command-line argument. A job step
running as the same user can read the argv of the runner process. The configuration is single-use
and the runner is ephemeral, which bounds the impact: it cannot be replayed once the job ends.

## Reporting

Report a vulnerability through GitHub private vulnerability reporting on this repository (the
Security tab, "Report a vulnerability"). Do not open a public issue for a security report.
