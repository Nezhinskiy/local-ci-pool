#!/bin/sh
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
# LOCAL_CI_MOUNT, LOCAL_CI_ROOT and LOCAL_CI_CA_PATHS exist for the tests.

set -eu

mount_dir=${LOCAL_CI_MOUNT:-/opt/local-ci}
root_dir=${LOCAL_CI_ROOT:-/tmp/runner}
ca_paths=${LOCAL_CI_CA_PATHS:-/etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt}

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

# 4. The runner runs as a child, so TERM and INT can be forwarded and the exit
# status returned. A shell starts a background command with SIGINT ignored, and
# an ignored signal cannot be trapped, so an INT sent to the child would be lost
# and this script would wait forever. INT is therefore forwarded as TERM, which
# the runner treats the same way: it stops the job and exits.
child=
trap 'if [ -n "$child" ]; then kill -s TERM "$child" 2>/dev/null || true; fi' TERM INT

if [ "$icu" = 1 ]; then
	"$root_dir/run.sh" --jitconfig "$local_ci_jit" &
else
	env DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$root_dir/run.sh" --jitconfig "$local_ci_jit" &
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
