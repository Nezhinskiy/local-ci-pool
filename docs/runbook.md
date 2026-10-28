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

Requirements: macOS; Docker Engine 28 or newer (verified on Docker Desktop) with at least 6 GB of
memory and 2 CPUs; `gh` 2.49 or newer (the installer verifies the release with `gh attestation`),
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

To take CI jobs' cores from the machine's core budget as well, add `--budget-cpu 2`; see
[the budget settings](#the-budget-settings).

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
7. with `--budget-cpu N`, creates the budget settings file when the pool would read none, and
   otherwise names the one it reads (see [the budget settings](#the-budget-settings));
8. runs `launchctl bootstrap` (retried a few times) and waits until `/healthz` reports the version it
   just installed. If an older version answers, or the new pool logged `terminal:` (for example
   `already running`), it prints a `WARNING`, restarts the agent once with
   `launchctl kickstart -k gui/$UID/com.local-ci-pool.pool`, and fails if that did not help. A first
   start downloads the runner and builds images, so a pool that has not answered within a minute is
   only reported, not treated as a failure. Once the new pool answers, the installer checks that it
   applies the budget settings, and warns when it does not (see
   [the budget settings](#the-budget-settings)).

The pool writes no configuration. `config.toml` is yours; the installer creates it only when asked.
The pool keeps derived state only: its git mirrors under
`~/Library/Caches/local-ci-pool`, the Docker images `local-ci-pool/runner:<version>` (the runner
trees) and `local-ci/<identity>:<digest>` (the job images), the Docker network
`local-ci-pool-main` that every job container joins, and the health token
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
budget settings file stays too, and the installer prints its path.

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
| `slots`, `in_use`, `busy` | the slot limit; slots held by runners, including runners a killed previous run left running; runners known to be running a job, which excludes those left running |
| `docker` | whether Docker answers |
| `login` | `false` while the last read of the token from `gh` failed (see [Install](#install)); `/healthz` answers 503 then too |
| `probe` | whether this is a probe run |
| `budget` | the core budget: `state`, an `error` class when there is one, and `oldest_wait_s` (see [CI is waiting](#ci-is-waiting)) |
| `projects` | one entry per project: `repo`, `identity`, `image`, `healthy`, a `reason` and its `class` (`starting`, `draining`, `setup`, `down`, `image`, `preflight` or `runners`) when not healthy, the budget's `waiting` and `admitted` counts, and `stuck` when a job is queued that GitHub does not offer the pool (see [A job stays queued on a free pool](#a-job-stays-queued-on-a-free-pool)) |
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
   pool v0.5.0 (8eda50a) on examplemacbo: docker ok, login ok, 7 slots, 0 in use, budget on
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
   - `login lost`: `gh` cannot give the pool a token. The pool keeps retrying and writes no
     heartbeat meanwhile. Run `gh auth status`; if `gh` is logged out, run `gh auth login`.
   - `unhealthy: <reason>` under a project: that project writes no heartbeat; the reason says why (see
     [Opting a project in or out](#opting-a-project-in-or-out)). `unhealthy: starting` is a project
     whose image and preflight are still on their way, as on a first start.
   - The repository is not listed at all: the pool does not serve it (see
     [A repository is not served](#a-repository-is-not-served)).
   - `N waiting for cores`: the budget holds the jobs (see [CI is waiting](#ci-is-waiting)).
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

## The budget settings

The core budget is optional. When the settings file turns it on, each runner waits for its cores in
the machine-wide [coreslot](https://github.com/core-slot/coreslot) budget, which local work shares,
before its container starts. Without the file the pool runs as it always has.

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
any), and the installer does not follow it. A pool started by hand (probe mode) resolves with its shell's `XDG_CONFIG_HOME`. `pool.log` logs `budget settings` with the `path` at start, and
again whenever the resolution changes; `/healthz` never shows the path.

**Keys.** One table, `[budget]`, with exactly two keys. Key names are case-sensitive: `CPU = 2` is an
unknown key, and makes the file invalid. So does a table spelled `[Budget]`, or `budget` in any
other case. Keys in other tables are ignored.

| Key | Value |
|---|---|
| `enabled` | `true` or `false` |
| `cpu` | the most cores one job asks for, a whole number from 1 to 256. It has no default: `enabled = true` without `cpu` is invalid |

```toml
[budget]
enabled = true
cpu = 2
```

Start at `cpu = 2`, the CPUs per slot the pool's sizing assumes. A job asks for `1..cpu`: it is
admitted as soon as one core is free and takes up to `cpu` (a `cpu` above the whole budget takes
at most the whole budget). The grant is fixed for the job's life, and `pool.log` logs it as
`granted` beside the `need` asked.

**The grant reaches the job** as `CORESLOT_CPU` in its environment (and as the container label
`local-ci-pool.need`). A workflow sizes its parallelism to it, for example
`pytest -n "${CORESLOT_CPU:-2}"` or `make -j "${CORESLOT_CPU:-2}"`; the default covers a job that
runs without the budget, where the variable is unset. The container is capped at its grant (Docker's
`--cpus`), so a job that ignores `CORESLOT_CPU` runs slower rather than oversubscribing the Mac.
Before this release a job always got all of `cpu`, so a workflow that assumed that must read
`CORESLOT_CPU` now. Without the budget, each container is capped at its slot's share of the Docker
VM's CPUs (CPUs divided by slots), and has a limit of 4096 processes and threads either way.

**Creating it.** `./install.sh --budget-cpu 2` creates `~/.config/local-ci-pool/config.toml` (mode
0600) when the installed pool would read no file, and never changes one that exists: it prints
`the budget settings exist; edit <path>` instead, naming the file the pool reads (a symlink to a
missing file at the `~/.config` path is refused too, never written through). It warns when its own
`XDG_CONFIG_HOME` names another directory, which the installed pool does not read, if you passed
`--budget-cpu` or a settings file sits under that directory. `--dry-run` prints what it would write.
After any install, the installer warns when the new pool's `/healthz` reports the file invalid
(`budget.error = "config"`), and when the file seems to turn the budget on (the installer only
looks for a line setting `enabled = true`; it parses no TOML) but `/healthz` shows it off (a path
the pool does not read, or a release before the budget, which ignores the file).
`--uninstall` leaves the file and prints its path.

**Edits apply without a restart.** The pool reads the file on every decision, with no cache. A new
`cpu` applies to the next runner; a runner already waiting keeps its place in the queue and the
range it asked for. Turning the budget on or off applies to the next runner; nothing waiting or
running is cancelled. Since the pool may read the file at any moment, save it by replacing it in
one step: write a temporary file in the same directory, then rename it over `config.toml`. A
half-written file is read as it is, which can make it invalid, turn the budget off or ask a
truncated `cpu`, and a file briefly absent turns the budget off and forgets the last valid
settings.

**An invalid file** is one that is not a regular file (a directory, a FIFO), is larger than 64 KiB,
cannot be read, has more than 64 dots, braces and brackets outside its comments and strings (a
bound on nesting, which the TOML decoder's cost grows with), is not valid TOML, has another key
under `[budget]`, a value of the wrong type, `enabled = true` without `cpu`, or a `cpu` outside 1
to 256. The last valid settings stay in force,
`/healthz` shows `budget.error = "config"`, and `pool.log` logs `budget settings invalid` with the
error. A file that is invalid when the pool starts leaves the budget off, with the same error. The
pool never exits over the file.

**Off.** No file, an empty file, no `[budget]` table, or `enabled = false` leaves the budget off.
Deleting the file turns it off and forgets the last valid settings.

**Labels and priority.** The pool asks for each job's cores under the label `<identity>:ci` (for
`alpha`, `alpha:ci`), always at priority 5. coreslot orders its queue by arrival less the priority
in minutes, so a CI job goes ahead of local work that queued up to five minutes after it. Five
minutes is GitHub's window: it completes a job request assigned to the pool that no runner took
within 5 minutes (`job completed` with `result=canceled` and no runner), and a runner still
waiting for cores by then has lost its job. A `[priorities]` entry in coreslot's own configuration
does not apply to these labels.

**Inside `coreslot run`.** A pool started by hand inside `coreslot run` would inherit that run's
grant. It drops it first, before it opens anything, budget on or off: it closes the grant's
descriptors and unsets the grant's variables (`CORESLOT_HELD_FD`, `CORESLOT_TOKEN_FDS`,
`CORESLOT_UNBUDGETED` and every `CORESLOT_GRANTED_*`), then queues as itself.

**Survivors.** A job container the previous pool left running (see
[launchd's 60-second limit](#launchds-60-second-limit)) carries its project as a label, and its need
as another when the budget admitted it (in any mode but `off`).
With the budget on, the new pool takes a hold in the budget for each one shortly after it starts,
and `pool.log` logs `runner admitted` with `mode=adopted`. A survivor that does not fit is not
stopped, since a running job cannot be paused: `pool.log` warns `survivor oversubscribed` with the
shortfall, and the hold is kept. A survivor of a release before the budget carries no project or need
label and is adopted as `local-ci-pool:ci` with the configured `cpu`; in `/healthz` it counts under no
project's `admitted`. From the old pool's exit until the adoption, survivors run outside the budget.
A survivor's job still counts among its repository's assigned jobs, and the pool counts the
project's survivors against them, so it starts no runner beside a survivor for the job the survivor
already runs. A survivor of a release before the budget names no project, so its project may get one
idle runner until that job ends.

**Rolling back.** Set `enabled = false`, or delete the file: the next runner starts without the
budget, with no restart. To leave the feature entirely, install the release before it with
`./install.sh --version v0.1.1`, which ignores the file.

## CI is waiting

With the budget on, a runner waits until the budget has its cores, however long that takes: a full
budget never lets a runner start without them. Its job shows as queued on GitHub meanwhile.

`/healthz` shows where things stand:

| Field | Meaning |
|---|---|
| `budget.state` | `off`, `on`, or `wedged` (below) |
| `budget.error` | absent when there is none; `config` (the settings file is invalid), `open` (the pool could not open coreslot's state; it retries after a backoff of up to a minute), or `state` (coreslot's state failed during the last admission, for example its state directory was removed) |
| `budget.oldest_wait_s` | the seconds the oldest waiting runner has waited |
| `waiting`, per project | the project's runners waiting for cores |
| `admitted`, per project | the project's runners and survivors holding an admission now: `queued` (admitted through the queue), `adopted` (survivors), `fail_open` (started without a grant) |

`pool.log` logs `runner waiting for the budget` when a runner queues, `runner admitted` (with
`mode`, `need`, `granted`, `shortfall` and `wait_s`) when it starts, and
`runner given up before admission` when it was given up before it started (`reason=cancelled`: no
longer needed; `reason=coreslot_cancel`: cancelled through coreslot). A runner is no longer needed when
its repository's assigned jobs have stayed below its waiting runners for 30 seconds: GitHub
completes an assigned job request that no runner took within 5 minutes (`job completed` with
`result=canceled` and `runner=""`) and reissues it about 0.3 seconds later, and the brief dip in
between gives no waiting runner up, so the reissued job finds its runner still in the queue. The
pool logs `assigned jobs below the waiting runners; keeping them for the grace` when such a dip
begins. Once the oldest runner has waited
30 minutes, it warns `budget full`, once every 30 minutes, with `oldest_wait_s` and the three
largest holds as `holds="<label> cpu=<n>, …"`. `/healthz` never names a hold.

To get CI moving:

1. See who holds the cores: `coreslot status` lists the holds and the queue, and the `budget full`
   line names the largest holds.
2. End the holder, or let it finish.
3. Or set `enabled = false` in the settings file: the next runner starts without the budget. A
   runner already waiting keeps its ticket; withdraw it with coreslot, and the runner is given up
   without starting (`reason=coreslot_cancel`) and the pool starts another for the job, which now
   runs without the budget. `coreslot cancel --label <identity>:ci` works when that runner is the
   project's only waiting one. All of a project's runners share the label, so with several waiting
   it refuses and names their ticket ids (the `id` column of `coreslot status`); cancel each with
   `coreslot cancel <ticket-id>`.

**Wedged.** `budget.state` is `wedged` when one process has held coreslot's admission section for 2
minutes, with no admission going through it meanwhile. A healthy client holds it for milliseconds,
so the holder is stopped (for example by Ctrl-Z) or stuck, and nobody can be admitted, local gates
included. `pool.log` warns `budget wedged` with `recorded_pid`, the pid in `admit.lock`'s
informational body, and `self=true` when that pid is the pool's own (`recorded_pid=0`: the body
could not be read). The body may be stale: a client that does not rewrite it leaves the previous
holder's in place, often the pool's own. Before stopping anything, find the real holder with
`lsof "<state directory>/admit.lock"`; `coreslot status` prints the state directory, and its
`admission section` line comes from the same body. Meanwhile the pool's runners start without a
grant: resume the holder (`kill -CONT <pid>`) or end it. The wedge clears at the next check that
finds the section free or held by another, and `pool.log` logs `budget unwedged`.

**Started without a grant.** When coreslot itself fails (it cannot be opened, its state fails, or it
is wedged), a runner starts anyway rather than block CI: `pool.log` warns `runner admitted` with
`mode=fail_open` and a `class` saying why, and `/healthz` counts it in `admitted.fail_open`.
`coreslot status` does not list such a runner, so its cores are not counted in the budget.

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

GitHub completes a job request assigned to the pool that no runner took within 5 minutes and
reissues it (see [Labels and priority](#the-budget-settings)). After such a reissue it has been
seen to stop counting the job against the pool's scale set: the job stays queued on GitHub, every
slot is free, and the pool never hears of it. The pool watches for this and only reports it; it
cancels and re-runs nothing.

When a job with the project's label (`<identity>-local`, or the scale set's name) has been queued
for 10 minutes while the pool had a free slot and the project had no runner waiting, starting or
idle, `pool.log` warns `a job is queued while the pool has a free slot and no runner waits for it`
with the `run` URL, the `job` name and `queued_s`, once per job, and the project's `/healthz` entry
lists it under `stuck` (`run_url`, `job`, `queued_s`) until it is no longer queued or the pool gets
busy. The report does not change `healthy`.

The pool asks GitHub only while those conditions have held for 10 minutes, at most every 5 minutes
per project, with at most 12 requests a check (the 10 newest queued and in-progress runs). A check
that fails (a rate limit, a network error) logs at debug only and changes nothing.

To get the job moving, cancel the run and choose **Re-run all jobs**: the new attempt's jobs are
offered to the pool as new requests.
