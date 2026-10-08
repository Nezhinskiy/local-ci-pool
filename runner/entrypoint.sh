#!/bin/bash
# The pool's entrypoint for a job container.
#
#   entrypoint.sh              read one JIT configuration line from stdin, run
#                              the runner once, exit with its status
#   entrypoint.sh --preflight  check that this image can run the runner and
#                              print the runner version
#
# The runner tree is mounted read-only at /opt/local-ci. The runner writes next
# to its binaries, so the tree is copied to a writable root first; a symlinked
# root fails because the runner resolves its root through bin/.
#
# The JIT configuration lives in one unexported shell variable and reaches the
# runner as an argument only. It is never exported, so no process environment
# carries it.
#
# LOCAL_CI_MOUNT, LOCAL_CI_ROOT and LOCAL_CI_CA_PATHS exist for the tests and
# are honoured only when LOCAL_CI_TEST_HOOKS=1 is also set. A job image can set
# any ENV it likes, so without that sentinel an image-controlled variable could
# redirect the runner root or fake a CA bundle.

set -eu

mount_dir=/opt/local-ci
root_dir=/tmp/runner
ca_paths="/etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt"
if [ "${LOCAL_CI_TEST_HOOKS:-}" = 1 ]; then
	mount_dir=${LOCAL_CI_MOUNT:-$mount_dir}
	root_dir=${LOCAL_CI_ROOT:-$root_dir}
	ca_paths=${LOCAL_CI_CA_PATHS:-$ca_paths}
fi

mode=job
case "${1:-}" in
'') ;;
--preflight) mode=preflight ;;
*)
	echo "local-ci: usage: entrypoint.sh [--preflight]" >&2
	exit 2
	;;
esac
if [ "$#" -gt 1 ]; then
	echo "local-ci: usage: entrypoint.sh [--preflight]" >&2
	exit 2
fi

# 1. The JIT configuration, from stdin, before anything else.
unset local_ci_jit
if [ "$mode" = job ]; then
	IFS= read -r local_ci_jit || [ -n "${local_ci_jit:-}" ] || true
	if [ -z "${local_ci_jit:-}" ]; then
		echo "local-ci: no JIT configuration on stdin" >&2
		exit 2
	fi
fi

# 2. A writable runner root. cp -a keeps modes and symlinks; failing to keep
# the owner is not an error for a non-root user. The root's own mode is copied
# too, so make it writable for the owner again.
if [ ! -d "$mount_dir" ]; then
	echo "local-ci: runner tree $mount_dir not found" >&2
	exit 2
fi
mkdir -p "$root_dir"
cp -a "$mount_dir"/. "$root_dir"/
chmod u+w "$root_dir"

# 3. Without libicu the runner aborts at start, so it runs in invariant mode.
# The variable is set for the runner child only, through env, never exported.
has_icu() {
	for ldconfig in ldconfig /sbin/ldconfig /usr/sbin/ldconfig; do
		if command -v "$ldconfig" >/dev/null 2>&1; then
			"$ldconfig" -p 2>/dev/null | grep -q libicu
			return
		fi
	done
	return 1
}
icu=0
if has_icu; then
	icu=1
fi

if [ "$mode" = preflight ]; then
	# A runner in an image with no CA bundle retries its TLS handshake forever
	# while `--version` still passes, so the bundle is part of the check.
	ca_ok=
	# shellcheck disable=SC2086 # the list is meant to be split
	for ca in $ca_paths; do
		if [ -s "$ca" ]; then
			ca_ok=1
		fi
	done
	if [ -z "$ca_ok" ]; then
		echo "local-ci: image has no CA bundle" >&2
		exit 3
	fi
	if [ "$icu" = 1 ]; then
		exec "$root_dir/bin/Runner.Listener" --version
	fi
	exec env DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$root_dir/bin/Runner.Listener" --version
fi

# 4. The runner runs as a child, so TERM can be forwarded and the exit status
# returned. Measured against the real run.sh (v2.338.0): unless
# RUNNER_MANUALLY_TRAP_SIG is set it runs run-helper.sh in the foreground with no
# trap, so a TERM sent to run.sh kills its bash, orphans run-helper.sh and
# Runner.Listener, and run.sh reports 0 for almost every listener status. With
# the variable set (for the child only, like the invariant flag) run.sh puts the
# helper in its own process group, sends INT to that group on TERM or INT, waits,
# and returns the helper's status, so the listener gets the signal and its status
# comes back. INT is forwarded to run.sh as TERM, which it handles the same way.
#
# set -m matters: without job control a shell starts a background command with
# SIGINT ignored, an ignored signal cannot be trapped, and that disposition is
# inherited by run.sh's children. (This is why the script is bash, not POSIX sh:
# dash refuses `set -m` without a tty. The runner's own run.sh needs bash too.) The INT that run.sh sends to the helper's group
# would then be dropped by the listener and the container would hang until
# SIGKILL (measured). With job control the child starts with default signals.
# The runner never reads stdin, so it gets /dev/null.
child=
set -m
trap 'if [ -n "$child" ]; then kill -s TERM "$child" 2>/dev/null || true; fi' TERM INT

if [ "$icu" = 1 ]; then
	env RUNNER_MANUALLY_TRAP_SIG=1 "$root_dir/run.sh" --jitconfig "$local_ci_jit" </dev/null &
else
	env RUNNER_MANUALLY_TRAP_SIG=1 DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$root_dir/run.sh" --jitconfig "$local_ci_jit" </dev/null &
fi
child=$!

# A trapped signal makes wait return early with a status above 128. Wait again
# until the child is gone, then report its real status.
status=0
while :; do
	if wait "$child"; then
		status=0
	else
		status=$?
	fi
	if ! kill -0 "$child" 2>/dev/null; then
		break
	fi
done
exit "$status"
