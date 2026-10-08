# Runbook

How to install, run and repair the pool on a Mac. Read [SECURITY.md](../SECURITY.md) first: the pool
holds your `gh` login and runs the code of your private repositories.

## Concepts you will meet

- **Identity.** A repository's name reduced to `[a-z0-9-]`, for example `alpha`.
- **Machine.** This Mac's `LocalHostName`, lower-cased and cut to its letters and digits, at most 12
  characters. `Example-MacBook` is `examplemacbo`. Change it in System Settings > General > Sharing >
  Local hostname.
- **Labels.** Workflows target the shared label `<identity>-local`. Each Mac registers its own scale
  set, `<identity>-<machine>`, and every scale set also carries the shared label, so GitHub gives a job
  to any Mac that is up. In probe mode both names carry a `probe-` prefix.
- **Marker.** `.github/local-ci.json` on a repository's default branch opts the repository in (see the
  [README](../README.md#the-marker)).
- **Heartbeat.** Every 120 seconds the pool sets the repository variable `CI_POOL_HB_<MACHINE>` to
  `"<unix epoch> <slots>"`, but only for a project that is healthy, only while Docker answers, and
  never in probe mode. The `route` action reads it and sends the expensive jobs to the Mac while a
  heartbeat is fresh (at most 300 seconds old).
- **Slots.** How many jobs one Mac runs at once:
  `min(floor((round(GiB) - 2) / 4), floor(CPUs / 2), 8)`, where `GiB` and `CPUs` are what Docker
  reports. The pool refuses to start below 1.

## Install

Requirements: macOS, Docker Desktop, `gh` logged in with the scopes the repositories need (`repo`,
`workflow`, `read:org`).

```sh
gh auth status
docker info
./install.sh            # or: ./install.sh --version v0.1.0
```

Run `./install.sh --dry-run` first if you want to see what it would do. The installer:

1. refuses to run as root, checks `gh auth status`, `docker info` and the slot count, and picks the
   archive for this Mac: `arm64` on any Apple Silicon Mac (even from a shell under Rosetta), `amd64`
   on an Intel Mac;
2. downloads the release archive with `gh release download` and runs `gh attestation verify` on it,
   requiring that it was signed by this repository's `release.yml` workflow for the very tag it
   downloaded. If the verification fails, nothing is installed;
3. stages the new binary as `pool.new` beside the old one, and renders the agent to
   `com.local-ci-pool.pool.plist.new` (logs go to `~/Library/Logs/local-ci-pool/pool.log`), checking it
   with `plutil -lint`. Everything that can fail does so here, while the running pool is untouched;
4. runs `launchctl bootout gui/$UID/com.local-ci-pool.pool` and then polls `launchctl print` until
   launchd no longer lists the agent. The pool finishes its running jobs first (up to 45 minutes,
   the plist's `ExitTimeOut`), and the installer prints a line every minute while it waits. It does
   not trust `bootout` to block: if the agent is still there after 46 minutes it stops, and no
   installed file has been changed;
5. renames the staged files into place (`~/Library/Application Support/local-ci-pool/bin/pool` and
   `~/Library/LaunchAgents/com.local-ci-pool.pool.plist`);
6. runs `launchctl bootstrap` (retried a few times) and waits for `/healthz`.

It writes no configuration. The pool keeps derived state only: its git mirrors under
`~/Library/Caches/local-ci-pool`, and the Docker images `local-ci/runner:<version>` and
`local-ci/<identity>:<digest>`. Deleting any of it costs a fetch or a build.

The agent restarts the pool when it exits with an error (`KeepAlive { SuccessfulExit = false }`, at
most one start per 30 seconds). A cause that restarting cannot fix, such as a logged-out `gh`, makes
the pool log `terminal: <reason>` and exit 0, so launchd does not spin; fix the cause and run the
installer again (or `launchctl kickstart gui/$UID/com.local-ci-pool.pool`).

## Upgrade

Run `./install.sh` again (or with `--version`). It stages the new files, stops the old pool, waits
until its jobs have finished and launchd has let go of it (up to 46 minutes), then renames the staged
files into place and starts the new pool. If anything fails after the old pool was stopped, the
installer says `the old pool is stopped and the new one is not running`; run it again. Workflows use
hosted runners meanwhile. A restarted pool may wait 21 to 39 seconds
for its previous session to expire: a `409` on opening the session is retried for up to 3 minutes. A
`409` beyond that means another pool, or another Mac with the same machine name, holds the scale set,
and the pool exits (terminal).

## Uninstall

```sh
./install.sh --uninstall
```

It stops the pool and waits until launchd has let go of it (the running jobs finish first), then runs
`pool forget` (deletes `CI_POOL_HB_<MACHINE>` from every repository the pool serves; other Macs'
variables are never touched), then removes the plist and the binary. The order matters: the heartbeat
dies with the process, so a variable deleted before the pool is gone could be written again. Workflows
fall back to hosted runners within five minutes, since a heartbeat is fresh for no longer than that.
Logs, the cache and the Docker images stay; delete them by hand if you want the space back.

## Adding a machine

1. Docker Desktop > Settings > Resources: set the memory. A 10 GB slider gives 2 slots, 18 GB gives 4
   (with 10 CPUs). Below 6 GB the pool refuses to start.
2. Docker Desktop > Settings > General: tick "Start Docker Desktop when you sign in".
3. `gh auth login`.
4. Run the installer.
5. Check [`/healthz`](#health-and-logs) and, within two minutes, the `CI_POOL_HB_<MACHINE>` variable
   in a served repository.

Two Macs whose names agree in their first 12 letters and digits collide on the same scale set; the
second one exits with a terminal error. Give one of them another `LocalHostName`.

## Changing slots

Change the memory slider in Docker Desktop. Docker restarts, the pool notices that the Docker VM
changed, exits, and launchd starts it again with the slot count derived anew. Jobs running at that
moment are lost with the containers; a job assigned to the pool at that time is requeued by GitHub.

## Opting a project in or out

- **In:** commit `.github/local-ci.json` to the repository's default branch. The pool finds it on its
  next discovery pass (every 10 minutes), builds or pulls the image, runs a preflight, and starts a
  listener. The project shows in `/healthz`; its heartbeat appears once it is healthy.
- **Out:** remove the marker from the default branch. The pool drains the project (running jobs
  finish, no new ones start) and deletes its scale set. A repository that turns public is drained
  the same way.

A job image must provide `bash`, glibc and a CA bundle. A bare `ubuntu:24.04` has no CA certificates:
install `ca-certificates`. The preflight reports which of the three is missing, and the project stays
unhealthy (so its workflows use hosted runners) until the image is fixed.

## Health and logs

```sh
curl -s 127.0.0.1:8737/healthz
tail -f ~/Library/Logs/local-ci-pool/pool.log
```

`/healthz` answers 200 while Docker answers and 503 otherwise, with the same JSON body:

| Field | Meaning |
|---|---|
| `version`, `commit` | the running build |
| `machine` | the machine name |
| `slots`, `in_use`, `busy` | the slot limit, slots held by runners, runners running a job |
| `docker` | whether Docker answers |
| `probe` | whether this is a probe run |
| `projects` | one entry per project: `repo`, `identity`, `image`, `healthy`, and a `reason` when not healthy |

The endpoint binds `127.0.0.1:8737` and answers only requests whose `Host` is a loopback name.
Binding it is also the single-instance lock: a second pool on the same Mac exits with
`terminal: already running`. The log is not rotated; stop the pool and delete the file when it grows
large.

A job assigned to a Mac that then went to sleep or lost power is requeued by GitHub to another live Mac
after about five minutes. With no other Mac, the run waits; see [stranded runs](#stranded-runs).

## Probe mode

Probe mode serves one repository under `probe-` names, writes no heartbeat, and so cannot attract the
real workflows. Use it to try a project before opting it in (commit the marker on a branch and read it
there). The installed binary is not on your PATH, so give its full path:

```sh
"$HOME/Library/Application Support/local-ci-pool/bin/pool" run --probe --only-repo OWNER/NAME --marker-ref my-branch
```

Stop the installed pool first (`launchctl bootout gui/$(id -u)/com.local-ci-pool.pool`, and
`./install.sh` afterwards to load it again). A second pool beside the installed one would count the
same Docker memory twice and oversubscribe it, and the default health address is the single-instance
lock, so it would not start anyway. (`--health-addr` can move the endpoint, but only to a loopback
address.) `--marker-ref` is optional; without it the marker is read from the default branch.

## Stranded runs

A run whose local jobs were assigned to a Mac that then lost power or closed its lid can wait until
GitHub's queue limit. GitHub requeues such a job to another live Mac when there is one. Otherwise:

- A repository whose workflow can re-plan on dispatch: cancel the run, then dispatch the workflow again
  on the integration branch with its runner input set to hosted (`gh workflow run <workflow> --ref
  <branch> -f runner=hosted`). That plans every mutation and re-promotes the default branch.
- A repository whose workflow cannot: cancel the run, then choose **Re-run all jobs**. Re-running only
  the failed jobs reuses the stale route.

The specific commands live in each project's own runbook. There is no automatic watchdog in v0.1.0.
