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
  reports. The pool refuses to start below 1. Every project on the Mac shares them. Each scale set tells GitHub
  how many jobs it can take now: the free slots plus its own running jobs, or 0 while it drains. So
  while one project's jobs hold every slot, GitHub keeps another project's new jobs off this Mac
  until a slot frees (another Mac that is up can still take them). A new count reaches GitHub with
  the scale set's next message request, within about 50 seconds.

## Install

Requirements (the README's [table](../README.md#requirements) has the checks): macOS, tested on 27 only;
Docker Engine 28 or newer (verified on Docker Desktop) with at least 6 GB of
memory and 2 CPUs; `gh` 2.68 or newer (the installer verifies the release with `gh attestation verify --source-ref`, which
2.68 introduced),
logged in as the account that owns the repositories. The runner is mounted into job containers as
an image, which needs Engine API 1.48 (Docker 28): the installer refuses an older engine and says
why. The pool's own calls need only the `repo` scope; the README's
`gh auth login --scopes repo,workflow,read:org` adds `workflow` so that the same login can push
workflow files over HTTPS, and `read:org` is part of every `gh` login.

From a clone of this repository (`gh repo clone Nezhinskiy/local-ci-pool`):

```sh
gh auth status
docker version --format '{{.Server.Version}} (API {{.Server.APIVersion}})'
./install.sh            # or: ./install.sh --version vX.Y.Z
```

The installer always installs a published release (the latest, or `--version`), never the code of
the clone it runs from.

Under launchd `DOCKER_HOST` is unset, so the pool finds Docker as the `docker` CLI does: `DOCKER_HOST`
first, then the current docker context, then the first of the known socket paths that exists. It
chooses once, when it starts. The launchd job never sees a `DOCKER_HOST` exported in your shell, so
if your engine is reachable only that way, with no docker context and no known socket, the installer
checks that engine and the pool then cannot find it (`docker down` in `pool status`). Give the
`docker` CLI a context for that host, and the pool follows it:

```sh
docker context create NAME --docker host=$DOCKER_HOST && docker context use NAME
```

After you switch Docker runtimes while the old one keeps running, make
the pool choose again:

```sh
launchctl kickstart -k gui/$(id -u)/com.local-ci-pool.pool
```

`-k` stops the pool the way any stop that comes from launchd does, giving it at most 60 seconds (see
[launchd's 60-second limit](#launchds-60-second-limit), where that was measured); stop it with
`SIGTERM` as shown there if a job is running.

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

The pool writes no configuration, and neither does the installer: `config.toml` is yours (see
[Limiting concurrent jobs](#limiting-concurrent-jobs)).
The pool keeps derived state only: its git mirrors under
`~/Library/Caches/local-ci-pool`, the Docker images `local-ci-pool/runner:<version>` (the runner
trees) and `local-ci/<identity>:<digest>` (the job images), one Docker network per pool
instance, `local-ci-pool-<instance>` (`main`, or `probe-<owner>` for a probe), which every job
container of every project joins, with traffic between containers off and the default bridge's
MTU, and the health token
`~/Library/Application Support/local-ci-pool/health-<port>.token`. Deleting any of it costs a
fetch, a build or a restart. Releases before the runner trees moved kept them under
`local-ci/runner:<version>`, where a repository named `runner` had its job images too; the first
start of a newer pool removes the old trees there and leaves job images alone.

The agent restarts the pool when it exits with an error (`KeepAlive { SuccessfulExit = false }`, at
most one start per 30 seconds). A cause that restarting cannot fix makes the pool log
`terminal: <reason>` and exit 0, so launchd does not spin: a second pool (`already running`), too
little Docker memory for one slot, a `LocalHostName` without letters or digits, a scale set held by
another Mac, or a token GitHub refuses after the pool read it again (`gh login rejected`). Fix the
cause and run the installer again (or `launchctl kickstart gui/$UID/com.local-ci-pool.pool`).

The agent runs with `ProcessType` `Standard`: priority 20, below apps and the Docker VM (31), so jobs
and your own work come first. Agents installed by earlier releases ran as `Background`, which puts
the pool and every `gh` and `git` it starts in darwinbg: priority 4, with disk and network I/O
throttled. On a Mac loaded far past its cores that starves the pool: its GitHub calls hit their
30-second timeout, heartbeats included, and the Mac drops out of routing although it is up.
`Adaptive` is no better, since it lifts a job only while an XPC transaction is open and the pool has
none. Measured on macOS 27.2.1 with one probe agent per type running side by side, on a 14-core Mac
at a load of 113 to 166 for 9 minutes, each cycle a fixed 30 ms CPU task, a child process and a fresh
HTTPS request to `api.github.com`:

| `ProcessType` | cycles | CPU task, median / max | child, median / max | HTTPS, median / max |
|---|---|---|---|---|
| `Background` | 27 | 1.1 s / 55 s | 0.7 s / 15 s | 0.4 s / 10.6 s |
| `Adaptive` | 42 | 3.5 s / 21 s | 0.9 s / 9.7 s | 0.6 s / 4.9 s |
| `Standard` | 91 | 0.35 s / 1.0 s | 0.07 s / 0.9 s | 0.3 s / 0.9 s |
| `Interactive` | 101 | 0.04 s / 0.05 s | 0.01 s / 0.03 s | 0.16 s / 0.4 s |

`Interactive` is faster still, but it competes with apps at their own priority, and launchd's
documentation keeps it for jobs a user interface waits on. The pool itself is light: about 90
seconds of CPU in 31 hours. `launchctl print gui/$UID/com.local-ci-pool.pool` reports
`spawn type = daemon (3)` for `Standard` and `background (5)` for an agent from an earlier release;
run the installer again to replace the latter.

A token `gh auth token` cannot give is not one of those causes: the pool keeps running and retries.
`gh` exits 1 both when it is logged out and when its keychain lookup times out (60 seconds), which a
stalled Mac can make it do, so the failure does not prove a logout. On 2026-10-21, a pool that took
it for one exited 0 and stayed down for 42 hours with the login intact. At start the pool reads
again after 2 seconds, doubling up to every 5 minutes, and logs
`gh gives no token; retrying` each time; it probes Docker only once it has a token. Later, after a
refusal dropped the cached token, a failed read leaves the affected project down until its next
retry, or until the next discovery pass. Meanwhile `/healthz` answers 503 with `"login": false`, and
no heartbeat is written, so workflows use hosted runners. If `gh` really is logged out, run
`gh auth login`; the pool picks the token up on its next read, without a restart.

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
Logs, the cache and the Docker images stay; delete them by hand if you want the space back. The
settings file stays too, and the installer prints its path.

The pool deletes its scale sets while it drains. If it was not running when you uninstalled, or its
drain hit the bound, a scale set can be left: check each served repository's Settings > Actions >
Runners for a leftover `<identity>-<machine>` scale set and remove it there.

`gh auth logout` without revoking the token does not stop a running pool: it keeps the token it
read until it restarts. Restart it (or uninstall it), or revoke the token, to cut it off at once.

## Adding a machine

1. Docker Desktop > Settings > Resources: set the memory. A 10 GB slider gives 2 slots, 18 GB gives 4
   (with 10 CPUs). Below 6 GB the pool refuses to start.
2. Docker Desktop > Settings > General: tick "Start Docker Desktop when you sign in".
3. `gh auth login --scopes repo,workflow,read:org`, as the account that owns the repositories.
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
To run fewer jobs than the slots allow, without touching Docker, set a cap instead (see
[Limiting concurrent jobs](#limiting-concurrent-jobs)).

## Opting a project in or out

- **In:** commit `.github/local-ci.json` to the repository's default branch. The pool finds it on its
  next discovery pass (every 10 minutes), builds or pulls the image, runs a preflight, and starts a
  listener. The project shows in `/healthz`; its heartbeat appears once it is healthy.
- **Out:** remove the marker from the default branch. The pool drains the project (running jobs
  finish, no new ones start) and deletes its scale set. A repository that moves to another owner is
  drained the same way.
- **Making a served repository public:** stop the pool first (`./install.sh --uninstall`, or by hand
  as in [launchd's 60-second limit](#launchds-60-second-limit)). A running pool needs GitHub's
  answer that the repository is private, at most one minute old, before it starts a runner, and asks
  again while jobs are assigned. So for up to a minute after the change, a pull request from a fork
  could still target `<identity>-local` directly and run on this Mac. When a runner start or a
  listener call sees the change, the pool logs a WARN line, removes the project's job containers at
  once, busy ones and those a previous pool process left included, and deletes the scale set. While
  GitHub still counts a removed job, the delete is retried for up to three minutes; a scale set left
  after that has no session, so no runner starts for it. `/healthz` and `pool status` then list the
  project as unhealthy with the reason `the repository is no longer private`, until it is private
  and served again or the pool restarts. A discovery pass that sees the change first drains the
  project as for a removed marker; its capacity is zero from then on, so no new job reaches it.
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

## The docker class

A repository whose marker has a `docker` block ([docs/docker-class.md](docker-class.md)) gets a second
project, `<identity>-docker`, whose jobs each run in a throwaway Lima VM with a Docker daemon of their own.
What you see and do:

- **Install Lima** on every Mac that should take such jobs: `brew install lima`. A Mac without it takes no
  docker jobs and registers no `<identity>-docker` scale set; that is no fault. Its entry stays healthy with
  `"not_installed": true` (`pool status`: `not installed (no limactl)`), and the repository's container jobs
  are served as ever. Install Lima later and the next discovery pass serves the class.
  Upgrade the pool on every Mac before a repository commits the block: an older pool refuses the marker.
- **The first base build** takes several minutes (about five, with the template's image download) and
  9.4 GiB of disk. `pool status` shows the class as `<owner>/<repo> (docker)`, `starting` until the base
  is built. A workflow names `runs-on: <identity>-docker`; the routing action and the heartbeat do not
  cover it, so such a job waits for a Mac that serves it and never falls back to a hosted runner.
- **Disk:** the pool starts no VM while the volume of `$LIMA_HOME` (else `~/.lima`) has less than 15 GiB free; the job stays
  queued, and the class shows `docker vm: not enough free disk for a docker VM: … GiB free, needs 15`.
  Free the space; the next poll starts it. Clones a killed pool left are deleted at its next start.
- **Reasons in `/healthz`** for the entry with `"docker_class": true`: `docker base: …` (class `setup` while there is no base yet, `image` when a rebuild
  failed and the old base still serves); `docker vm: …` (class `vm`, the last VM launch failed; cleared by
  the next one that succeeds). One reason does not clear: a docker VM the pool could not delete (after a job,
  a failed boot, or in the start-up sweep) may still run, so the pool starts no docker VM and builds no base
  until it restarts; a drain's failed delete counts too. The runner of such a VM stays in use until then, so
  the Mac is kept awake, and the restart's drain waits for it up to its bound. Check `limactl list`, delete
  the VM by hand, then restart the pool.
- **Rebuilding the base by hand:** delete
  `~/Library/Application Support/local-ci-pool/docker-base-<identity>.key`; the next discovery pass (or a
  restart) builds it again. The base itself, `local-ci-docker-base-main-<identity>`, is protected in Lima.
- **After the block is removed** the pool stops the class but keeps its base (about 10 GiB) and its key
  file. To reclaim them: `limactl unprotect local-ci-docker-base-main-<identity>`, `limactl delete
  local-ci-docker-base-main-<identity>`, and delete `docker-base-<identity>.key`.

## A repository is not served

`pool status` lists every repository the pool serves. One that is missing was skipped on the last
discovery pass, which runs at start and then every 10 minutes. `pool.log` says why for most causes:

```sh
grep 'skipping repository' ~/Library/Logs/local-ci-pool/pool.log | tail
```

- **Public, or owned by an organization.** The pool lists only the private repositories the `gh`
  account owns, so these never reach it and no line names them. Public repositories are refused by
  design ([SECURITY.md](../SECURITY.md)); make the repository private, or serve it with another
  runner.
- **No marker on the default branch.** A marker on another branch is read only in
  [probe mode](#probe-mode). The pool logs this at debug level, which `pool.log` leaves out.
- **`skipping repository: invalid marker`**, with the reason: the JSON has another shape than
  [the README's](../README.md#the-marker), an image without `@sha256:` and its 64-hex digest, both
  forms at once, or a `dockerfile` outside its `inputs`.
- **`skipping repository: identity collision`**: two repositories reduce to the same identity
  (`my-repo` and `My_Repo`); the one named `kept` is served. Rename the other.
- **`skipping repository: the marker could not be read`** or
  **`the visibility re-check failed`**: a GitHub call failed; the next pass tries again.
- **Committed a moment ago.** Wait for the next pass, or restart the pool when no job is running:
  `launchctl kickstart -k gui/$(id -u)/com.local-ci-pool.pool`.

## Health and logs

```sh
"$HOME/Library/Application Support/local-ci-pool/bin/pool" status --json
tail -f ~/Library/Logs/local-ci-pool/pool.log
```

`/healthz` answers 200 while Docker answers and `gh` gives the pool a token, and 503 otherwise,
with the same JSON body. It answers from the moment the pool binds its address, also while it waits
for a token. A request without the pool's health token gets the redacted body, which a job can read
too (see below): `"redacted": true`, each `reason` cut to its `class`, and no `image` or `stuck`.
`pool status` sends the token; with `curl`, send it yourself:

```sh
curl -s -H "Authorization: Bearer $(cat "$HOME/Library/Application Support/local-ci-pool/health-8737.token")" 127.0.0.1:8737/healthz
```

The fields:

| Field | Meaning |
|---|---|
| `version`, `commit` | the running build |
| `machine` | the machine name |
| `slots`, `in_use`, `busy` | the slot limit, after the cap on jobs; slots held by runners, including runners a killed previous run left running; runners known to be running a job, which excludes those left running |
| `vm_in_use` | the docker class's one VM slot in use: 0 or 1 ([docs/docker-class.md](docker-class.md)) |
| `docker` | whether Docker answers |
| `login` | `false` while the last read of the token from `gh` failed (see [Install](#install)); `/healthz` answers 503 then too |
| `probe` | whether this is a probe run |
| `jobs` | the cap on jobs: `max` (0: none) and `error`, `config` while the settings file is invalid, absent otherwise (see [Limiting concurrent jobs](#limiting-concurrent-jobs)) |
| `projects` | one entry per project: `repo`, `identity`, `image`, `healthy`, a `reason` and its `class` (`starting`, `draining`, `setup`, `down`, `image`, `preflight`, `vm` or `runners`) when not healthy, `docker_class` on [the docker class](#the-docker-class)'s entry and `not_installed` on such an entry on a Mac without Lima (healthy: it takes no docker jobs), and `stuck` when a job is queued that GitHub does not offer the pool (see [A job stays queued on a free pool](#a-job-stays-queued-on-a-free-pool)) |
| `redacted` | `true` on the body served without the token; absent otherwise |

`pool status` reads the same endpoint and prints it as text (see [CI went hosted](#ci-went-hosted)).

The log leaves out the listener's per-poll line, `Getting next message`, which `actions/scaleset`
writes before every poll of every scale set and which would be nearly all of it. Its other line,
`Handling initial session statistics`, stays. `Acquiring jobs` and `Jobs acquired` are written by the
pool's scaler, with `scaleSetID`, before any runner of the job starts: they are the only record that
GitHub handed a scale set a job. A failed acquisition is logged at Warn as `acquiring jobs failed`, and
the listener keeps running: GitHub reissues an unacquired request after 5 minutes.

The endpoint binds `127.0.0.1:8737` and answers only requests whose `Host` is a loopback name. A job
can still reach it through Docker Desktop's host gateway address with a forged `Host`, and so learn
which repositories the pool serves and the class of each one's trouble, but not its reason; see
[SECURITY.md](../SECURITY.md). The pool writes a new token at each start, once it holds the
address, so a second pool that exits with `already running` leaves the first one's token alone.
When `pool status` cannot read the token, it says so under the projects and shows the classes. Binding it is also the
single-instance lock: a second pool on the same Mac exits with `terminal: already running`. The log is not rotated; stop the pool and delete the file when it grows
large.

A job assigned to a Mac that then went to sleep or lost power, and not yet started there, is requeued
by GitHub to another live Mac after about five minutes; with no other Mac, the run waits (see
[stranded runs](#stranded-runs)). A job that had already started is never requeued: it fails with a
lost runner and needs a re-run. A pool killed while Docker keeps running (`kill -9`, a crash,
[launchd's 60-second limit](#launchds-60-second-limit)) is different: a job that had started keeps
running in its container, and the next pool leaves it running, counts it in `in_use` until it exits,
and removes only this pool's containers that had already stopped.

## CI went hosted

A run used a hosted runner when you expected a Mac. The routing falls back whenever no Mac has a
fresh heartbeat, so the question is why this one did not.

1. **Ask the pool.** The installed binary is not on your `PATH`:

   ```sh
   "$HOME/Library/Application Support/local-ci-pool/bin/pool" status
   ```

   ```
   pool v0.6.0 (a4d3acb) on examplemacbo: docker ok, login ok, 3 slots, 0 in use, at most 4 jobs
     alice/alpha  healthy
     alice/beta   unhealthy: draining
       queued 12m0s: test https://github.com/alice/beta/actions/runs/1
   ```

   The exit status is 0 for a healthy pool, 1 when `/healthz` answers 503, and 3 when nothing
   answers; `--json` prints the body as it is. Read the first line, then the projects:

   - `pool: not running`: nothing answers on the health address. Look at
     `launchctl print gui/$(id -u)/com.local-ci-pool.pool` and the end of `pool.log`; a line starting
     `terminal:` names a cause the pool will not retry (see [Install](#install)).
   - `docker down`: Docker does not answer. No heartbeat is written until it does.
   - Healthy, but `pool.log` has `heartbeat write failed` with `Client.Timeout exceeded` or
     `i/o timeout`: the pool could not reach GitHub within 30 seconds. On a Mac loaded far past its
     cores, check that the agent is not `Background` (see [Install](#install)).
   - `login lost`: `gh` cannot give the pool a token. The pool keeps retrying and writes no
     heartbeat meanwhile. Run `gh auth status`; if `gh` is logged out, run `gh auth login`.
   - `unhealthy: <reason>` under a project: that project writes no heartbeat; the reason says why (see
     [Opting a project in or out](#opting-a-project-in-or-out)). `unhealthy: starting` is a project
     whose image and preflight are still on their way, as on a first start.
   - The repository is not listed at all: the pool does not serve it (see
     [A repository is not served](#a-repository-is-not-served)).
   - `at most N jobs`: a cap is set (see [Limiting concurrent jobs](#limiting-concurrent-jobs));
     `config invalid`: the settings file is invalid, and the last valid cap stays in force.
   - `queued <time>: <job> <run URL>`: GitHub has not offered the pool a job that has waited that
     long for a runner (see [A job stays queued on a free pool](#a-job-stays-queued-on-a-free-pool)).

2. **Read the run's annotation.** An `auto` route that falls back says so on the `route` job. The
   three lines, here for `hosted-label: ubuntu-24.04-arm`:

   ```
   ::notice title=local-ci-pool::no Mac heartbeat found; running on ubuntu-24.04-arm
   ::warning title=local-ci-pool::no fresh Mac heartbeat (the newest, CI_POOL_HB_EXAMPLEMACBO, is 1d 18h old); running on ubuntu-24.04-arm
   ::warning title=local-ci-pool::no fresh Mac heartbeat (the newest, CI_POOL_HB_EXAMPLEMACBO, is in the future); running on ubuntu-24.04-arm
   ```

   - The notice: the repository has no heartbeat variable. Check
     `gh variable list --repo OWNER/NAME`; a healthy pool writes `CI_POOL_HB_<MACHINE>` within two
     minutes.
   - The first warning: the newest heartbeat is too old (fresh is at most 300 seconds). It names the
     machine and its age; the pool has been quiet for that long, so go to that Mac and run
     `pool status`.
   - The second warning: the newest heartbeat is more than 30 seconds ahead of the runner's clock.
     Fix the Mac's clock.

   A run forced to `mode: hosted` prints none of them.

3. **An example of a pool that was down.** On 2026-10-21 the pool started at 03:29. Its first
   `gh auth token` ran until 03:55 and exited 1, and the pool took that for a logout: it logged
   `terminal: gh logged out` and exited 0, which launchd never restarts. It stayed down for 42 hours
   with the login intact. `gh` exits 1 both when it is logged out and when its keychain lookup times
   out, so that status proves nothing. The annotations did not exist yet; a run after 42 hours of
   that would show `is 1d 18h old` in the first warning above. Since v0.3.2 the pool does not exit
   on a token it cannot read: it retries, and `pool status` shows `login lost` while `gh` cannot
   give a token (see [Install](#install)).

## Limiting concurrent jobs

The pool runs as many container jobs at once as it has slots (see [Changing slots](#changing-slots)),
and one docker VM beside them. To run fewer, for example to leave the Mac room for local builds, set
a cap in the settings file. Without the file the pool runs as it always has.

Every container job is held to its share of the Docker VM's CPUs: the VM's CPUs divided by the cap, or
by the slots without a cap, at least 1 (with a cap of 4 on 14 CPUs, 3 each). A job that sizes its
workers by the CPUs it sees (`os.availableParallelism()`, `nproc` under the quota) then starts as many as
its share holds, not one per CPU of the whole VM inside its 4 GiB. A docker VM keeps its own 4 CPUs
and counts as one job.

**Where.** The pool resolves the path on every read:

1. `$XDG_CONFIG_HOME/local-ci-pool/config.toml` when `XDG_CONFIG_HOME` is set and absolute,
   otherwise `~/.config/local-ci-pool/config.toml`;
2. when that file does not exist (a symlink to a missing file counts as missing) and
   `~/Library/Application Support/local-ci-pool/config.toml` does, the latter (the macOS fallback,
   beside the installer's `bin/`).

The installed pool does not inherit your shell's environment (its launchd agent sets only `PATH`), so
an `XDG_CONFIG_HOME` set in a shell does not reach it, and it reads
`~/.config/local-ci-pool/config.toml`, or the fallback. A value set for launchd itself with
`launchctl setenv XDG_CONFIG_HOME` does reach it (`launchctl getenv XDG_CONFIG_HOME` prints it, if
any). A pool started by hand (probe mode) resolves with its shell's `XDG_CONFIG_HOME`. `pool.log`
logs `config file` with the `path` at start, and again whenever the resolution changes; `/healthz`
never shows the path. The installer never writes the file; it warns when its own `XDG_CONFIG_HOME`
names another directory that holds one, which the installed pool does not read, and `--uninstall`
leaves the file and prints its path.

**The key.** One table, `[jobs]`, with one key:

```toml
[jobs]
max = 4
```

`max` is the most jobs the pool runs at once on the Mac, container jobs and docker VMs together (a
docker base build is a VM too): a whole number of at least 1. The container slots become
`min(slots, max)`, and a docker VM takes one of them while it runs: with `max = 4`, at most three
container jobs run beside a docker job. `/healthz` shows the capped slots as `slots` and the cap as
`jobs.max`; `pool status` adds `at most 4 jobs` to its first line. The heartbeat advertises the
capped slots. Key names are case-sensitive: `Max = 4` is an unknown key and makes the file invalid,
and so does a table spelled `[Jobs]`. Other tables are ignored.

**Edits apply without a restart.** The pool reads the file at start and every 30 seconds after. A
raised cap starts the jobs that waited for it; a lowered one stops no running job, and starts none
until enough have ended. Save the file by replacing it in one step: write a temporary file in the
same directory, then rename it over `config.toml`. A half-written file can read as invalid, and a
file briefly absent removes the cap.

**An invalid file** is one that is not a regular file (a directory, a FIFO), is larger than 64 KiB,
cannot be read, has more than 64 dots, braces and brackets outside its comments and strings (a
bound on nesting, which the TOML decoder's cost grows with), is not valid TOML, has another key
under `[jobs]`, or a `max` that is not a whole number of at least 1 (`0`, `2.5`, `"4"`). The last
valid cap stays in force (none, when the file has been invalid since the start), `/healthz` shows
`jobs.error = "config"`, `pool status` adds `config invalid`, and `pool.log` warns
`config invalid; keeping the last valid settings` with the error, once per new error. The pool never
exits over the file.

**A `[budget]` table** left from a release with the core budget is not read: `pool.log` warns
`config: [budget] is no longer read; the core budget was removed, set [jobs] max instead` once, and
the pool runs on. Delete the table. The core budget gave each job a share of the Mac's cores through
coreslot and capped its container at that share; a job container is now capped at a static share
instead (above), `CORESLOT_CPU` is no longer set in a job's
environment, so a workflow that read `"${CORESLOT_CPU:-2}"` gets its default, and
`install.sh --budget-cpu` is gone.

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

The specific commands live in each project's own runbook.

## A job stays queued on a free pool

GitHub completes a job request assigned to the pool that no runner took within 5 minutes (`job
completed` with `result=canceled` and no runner) and reissues it. After such a reissue it has been
seen to stop counting the job against the pool's scale set: the job stays queued on GitHub, every
slot is free, and the pool never hears of it. The pool watches for this and only reports it; it
cancels and re-runs nothing.

When a job with the project's label (`<identity>-local`, or the scale set's name) has been queued
for 10 minutes while the pool had a free slot, within the cap on jobs, and the project had no
runner starting or idle, `pool.log` warns `a job is queued while the pool has a free slot and no runner waits for it`
with the `run` URL, the `job` name and `queued_s`, once per job, and the project's `/healthz` entry
lists it under `stuck` (`run_url`, `job`, `queued_s`) until it is no longer queued or the pool gets
busy. The report does not change `healthy`.

The pool asks GitHub only while those conditions have held for 10 minutes, at most every 5 minutes
per project, with at most 12 requests a check (the 10 newest queued and in-progress runs). A check
that fails (a rate limit, a network error) logs at debug only and changes nothing.

To get the job moving, cancel the run and choose **Re-run all jobs**: the new attempt's jobs are
offered to the pool as new requests.
