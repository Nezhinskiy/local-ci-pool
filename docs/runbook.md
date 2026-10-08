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
   downloaded (and, when this `gh` supports `--deny-self-hosted-runners`, on a GitHub-hosted
   runner). If the verification fails, nothing is installed;
3. stages the new binary as `pool.new` beside the old one, and renders the agent to
   `com.local-ci-pool.pool.plist.new` (logs go to `~/Library/Logs/local-ci-pool/pool.log`), checking it
   with `plutil -lint`. Everything that can fail does so here, while the running pool is untouched;
4. stops a running pool itself: it reads the pool's pid from
   `launchctl print gui/$UID/com.local-ci-pool.pool`, sends it `SIGTERM`, and waits until that
   process has exited. The pool finishes its running jobs first (up to 40 minutes, then at most 3
   more to clean up), and the installer prints a line every minute while it waits. If the pool is
   still draining after 46 minutes the installer stops there: nothing has been replaced, and the pool
   exits on its own once its drain ends. The installer does not leave the stop to launchd, which
   would kill a draining pool after a minute (see [launchd's 60-second limit](#launchds-60-second-limit));
5. runs `launchctl bootout gui/$UID/com.local-ci-pool.pool`, which is quick now that the process is
   gone, and polls `launchctl print` until launchd no longer lists the agent; then waits, with the
   same bound and progress lines, until nothing answers on the health address any more, so that the
   new pool can bind it;
6. renames the staged files into place (`~/Library/Application Support/local-ci-pool/bin/pool` and
   `~/Library/LaunchAgents/com.local-ci-pool.pool.plist`);
7. runs `launchctl bootstrap` (retried a few times) and waits until `/healthz` reports the version it
   just installed. If an older version answers, or the new pool logged `terminal:` (for example
   `already running`), it prints a `WARNING`, restarts the agent once with
   `launchctl kickstart -k gui/$UID/com.local-ci-pool.pool`, and fails if that did not help. A first
   start downloads the runner and builds images, so a pool that has not answered within a minute is
   only reported, not treated as a failure.

It writes no configuration. The pool keeps derived state only: its git mirrors under
`~/Library/Caches/local-ci-pool`, and the Docker images `local-ci/runner:<version>` and
`local-ci/<identity>:<digest>`. Deleting any of it costs a fetch or a build.

The agent restarts the pool when it exits with an error (`KeepAlive { SuccessfulExit = false }`, at
most one start per 30 seconds). A cause that restarting cannot fix, such as a logged-out `gh`, makes
the pool log `terminal: <reason>` and exit 0, so launchd does not spin; fix the cause and run the
installer again (or `launchctl kickstart gui/$UID/com.local-ci-pool.pool`).

## Upgrade

Run `./install.sh` again (or with `--version`). It stages the new files, sends the old pool
`SIGTERM`, waits until its jobs have finished and its process has exited (up to 46 minutes), unloads
the agent, then renames the staged files into place and starts the new pool. If the old pool is still
draining at the bound, the installer stops without replacing anything and says so; the pool exits once
its drain ends and is not restarted, so run the installer again after it has exited (`drained` in the
log). A run started while the pool still drains only waits for it: it never signals a draining pool
twice. If anything fails after the old
pool was stopped, or the installer is interrupted then, it says
`the old pool is stopped and the new one is not running`; run it again.

The drain is bounded at 40 minutes, which is not above every job timeout a served repository may
set. A job still running at the bound is stopped (its container is removed) on an upgrade or an
uninstall; it fails and needs a re-run. Upgrade when no long job is running. Workflows use
hosted runners meanwhile. A restarted pool may wait 21 to 39 seconds
for its previous session to expire: a `409` on opening the session is retried for up to 3 minutes. A
`409` beyond that means another pool, or another Mac with the same machine name, holds the scale set,
and the pool exits (terminal).

### launchd's 60-second limit

Measured on macOS 27.0.1 (build 26A434): launchd clamps a LaunchAgent's exit timeout to 60 seconds.
The plist asks for an `ExitTimeOut` of 2700, but `launchctl print gui/$UID/com.local-ci-pool.pool`
reports `exit timeout = 60`, and a `launchctl bootout` during an upgrade killed the pool (`SIGKILL`)
about 62 seconds into its drain, with a job still running. So every stop that comes from launchd gives
the pool at most 60 seconds: a logout, a shutdown or a reboot, and `launchctl bootout` or
`launchctl stop` by hand. A job still running then is not lost with the pool as long as Docker keeps
running: the next pool to start leaves its container running, holds a slot for it until it exits, and
removes only the containers that had already stopped (v0.1.0's start-up sweep removed the running ones
too, so the job was lost). A reboot or a shutdown stops Docker as well, and its jobs are lost anyway.

A `SIGTERM` sent from outside launchd has no such limit. Measured on the same Mac: the pool drained for
as long as its running job needed, the job finished green, the pool deleted its scale set and exited 0
after 462 seconds, and launchd did not restart it (`state = not running`, `last exit code = 0`). The
installer stops the pool this way. To stop it by hand without losing a job, do the same:

```sh
pid="$(launchctl print gui/$(id -u)/com.local-ci-pool.pool | awk '/^\tpid = / { print $3; exit }')"
kill -TERM "$pid"                                          # once: drains, then exits 0
launchctl kickstart gui/$(id -u)/com.local-ci-pool.pool    # start it again later
```

Send `SIGTERM` only once. After the first one the pool restores the default action, so a second
`SIGTERM` (or a Ctrl-C to a pool in a terminal) kills it at once, mid-drain. The installer reads the
pool's log first: if the running process already logged `stopping: draining every project` (after
its own `pool starting` line, which names its pid), it does not signal it again and only waits, so
running `./install.sh` again while the pool drains is safe.

## Uninstall

```sh
./install.sh --uninstall
```

It sends the pool `SIGTERM` and waits until its process has exited (the running jobs finish first, up
to 46 minutes; if the pool is still draining then, nothing is removed and the installer says so), then
unloads the agent and waits until launchd has let go of it, then runs `pool forget` (deletes `CI_POOL_HB_<MACHINE>` from every repository the pool serves; other Macs'
variables are never touched), then removes the plist and the binary. The order matters: the heartbeat
dies with the process, so a variable deleted before the pool is gone could be written again. Workflows
fall back to hosted runners within five minutes, since a heartbeat is fresh for no longer than that.
Logs, the cache and the Docker images stay; delete them by hand if you want the space back.

The pool deletes its scale sets while it drains. If it was not running when you uninstalled, or its
drain hit the bound, a scale set can be left: check each served repository's Settings > Actions >
Runners for a leftover `<identity>-<machine>` scale set and remove it there.

`gh auth logout` without revoking the token does not stop a running pool: it keeps the token it
read until it restarts. Restart it (or uninstall it), or revoke the token, to cut it off at once.

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

Each project's listener advertises the whole Mac's slot count, and the slots are shared by every
project. With several projects, a Mac can therefore accept more jobs at once than it has slots; the
extra jobs wait for a local slot instead of going to another Mac. This matters once a second Mac
serves the same label.

## Changing slots

Change the memory slider in Docker Desktop. Docker restarts, the pool notices that the Docker VM
changed, exits, and launchd starts it again with the slot count derived anew. Jobs running at that
moment are lost with the containers; a job assigned to the pool at that time is requeued by GitHub.

## Opting a project in or out

- **In:** commit `.github/local-ci.json` to the repository's default branch. The pool finds it on its
  next discovery pass (every 10 minutes), builds or pulls the image, runs a preflight, and starts a
  listener. The project shows in `/healthz`; its heartbeat appears once it is healthy.
- **Out:** remove the marker from the default branch. The pool drains the project (running jobs
  finish, no new ones start) and deletes its scale set. A repository that turns public, or moves to
  another owner, is drained the same way.
- **Making a served repository public:** stop the pool first (`./install.sh --uninstall`, or by hand
  as in [launchd's 60-second limit](#launchds-60-second-limit)). The pool notices the change on its next discovery pass,
  up to 10 minutes later, and in that window a pull request from a fork could target
  `<identity>-local` directly and run on this Mac.
- **Repository names:** the `route` action accepts identities of 1 to 40 characters. A repository
  whose identity is longer fails its route job visibly; rename it to use the pool.

A job image must provide `bash`, glibc and a CA bundle. A bare `ubuntu:24.04` has no CA certificates:
install `ca-certificates`. The preflight names a missing CA bundle; a missing `bash` or glibc shows
as an exec or loader error in the reason. The project stays unhealthy (so its workflows use hosted
runners) until the image is fixed.

A project is also unhealthy, with the reason in `/healthz`, while:

- its image changed and the new one failed to pull, build or pass the preflight. Runners keep using
  the last good image; the project recovers on the next discovery pass whose refresh succeeds;
- a new actions/runner release failed the preflight with its image. While the pool keeps running,
  runners stay on the older runner (its image is not pruned) until a later preflight with the new one
  passes, on the next hourly release check or the next discovery pass. A pool restart or upgrade
  keeps only the newest runner, so the project then cannot start at all: it stays unhealthy, with
  the runner version in the reason, and its workflows use hosted runners until the image is fixed
  or a runner release it passes with arrives;
- three runners in a row went away without starting a job (they exited idle, or could not start,
  for example because an image was deleted under the pool). A job report clears it, and so does the
  preflight the next discovery pass runs for it.

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
| `slots`, `in_use`, `busy` | the slot limit; slots held by runners, including runners a killed previous run left running; runners known to be running a job, which excludes those left running |
| `docker` | whether Docker answers |
| `probe` | whether this is a probe run |
| `projects` | one entry per project: `repo`, `identity`, `image`, `healthy`, and a `reason` when not healthy |

The endpoint binds `127.0.0.1:8737` and answers only requests whose `Host` is a loopback name. A job
can still reach it through Docker Desktop's host gateway address with a forged `Host`, and so learn
which repositories the pool serves; see [SECURITY.md](../SECURITY.md). Binding it is also the
single-instance lock: a second pool on the same Mac exits with `terminal: already running`. The log is not rotated; stop the pool and delete the file when it grows
large.

A job assigned to a Mac that then went to sleep or lost power, and not yet started there, is requeued
by GitHub to another live Mac after about five minutes; with no other Mac, the run waits (see
[stranded runs](#stranded-runs)). A job that had already started is never requeued: it fails with a
lost runner and needs a re-run. A pool killed while Docker keeps running (`kill -9`, a crash,
[launchd's 60-second limit](#launchds-60-second-limit)) is different: a job that had started keeps
running in its container, and the next pool leaves it running, counts it in `in_use` until it exits,
and removes only this pool's containers that had already stopped.

## Probe mode

Probe mode serves one repository under `probe-` names, writes no heartbeat, and so cannot attract the
real workflows. Use it to try a project before opting it in (commit the marker on a branch and read it
there). The installed binary is not on your PATH, so give its full path:

```sh
"$HOME/Library/Application Support/local-ci-pool/bin/pool" run --probe --only-repo OWNER/NAME --marker-ref my-branch
```

Stop the installed pool first (by hand as in [launchd's 60-second limit](#launchds-60-second-limit),
and `launchctl kickstart gui/$(id -u)/com.local-ci-pool.pool` afterwards to start it again). A second pool beside the installed one would count the
same Docker memory twice and oversubscribe it, and the default health address is the single-instance
lock, so it would not start anyway. (`--health-addr` can move a probe's endpoint, but only to a
loopback address, and only together with `--probe`: a second non-probe pool would skip the lock and
its start-up sweep and its reconcile would remove the installed pool's containers.) `--marker-ref` is optional;
without it the marker is read from the default branch.

## Stranded runs

A run whose local jobs were assigned to a Mac that then lost power or closed its lid can wait until
GitHub's queue limit. GitHub requeues a job that had not started to another live Mac when there is
one; a job that had started fails with a lost runner and needs a re-run. Otherwise:

- A repository whose workflow can re-plan on dispatch: cancel the run, then dispatch the workflow again
  on the integration branch with its runner input set to hosted (`gh workflow run <workflow> --ref
  <branch> -f runner=hosted`). That plans every mutation and re-promotes the default branch.
- A repository whose workflow cannot: cancel the run, then choose **Re-run all jobs**. Re-running only
  the failed jobs reuses the stale route.

The specific commands live in each project's own runbook. There is no automatic watchdog in v0.1.0.
