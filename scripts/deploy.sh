#!/usr/bin/env bash
# Deploys qswitch the way the README's systemd and launchd steps do by hand:
# a fresh clone of one commit, builds whose embedded version is that commit,
# the binaries copied next to the running ones and checked, swapped in with
# a backup kept, the daemon restarted and its health polled for the new
# version, the first quota probe awaited, and the backup removed. A daemon
# that does not come back on the new version within a minute is rolled back.
#
#   scripts/deploy.sh [--ref <commit>] [novix|macmini ...]
#
# --ref defaults to origin/main and the targets to both. Run it on the Mac
# mini: it needs go, ssh to novix, and launchctl for its own daemon. Managed
# Claude sessions on novix pick up the new qswitch by themselves when idle;
# the script reports how many have not yet.
# Command strings run on the target through ssh or bash -c, where the tilde
# and the dollar expressions expand.
# shellcheck disable=SC2088,SC2016
set -euo pipefail

usage() { echo "usage: $0 [--ref <commit>] [novix|macmini ...]"; }
die() { echo "deploy: $*" >&2; exit 1; }

ref=origin/main
targets=()
while [ $# -gt 0 ]; do
	case "$1" in
	--ref) [ $# -ge 2 ] || die "--ref needs a commit"; ref=$2; shift 2 ;;
	--ref=*) ref=${1#--ref=}; shift ;;
	novix | macmini) targets+=("$1"); shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
[ ${#targets[@]} -gt 0 ] || targets=(novix macmini)

# The hosts: how to reach them, what to build for them, how to restart the
# daemon, and how to hash a file there. macmini is the machine this runs on.
ssh_host() { case $1 in novix) echo novix ;; macmini) echo "" ;; esac; }
goos() { case $1 in novix) echo linux ;; macmini) echo darwin ;; esac; }
goarch() { case $1 in novix) echo amd64 ;; macmini) echo arm64 ;; esac; }
restart_cmd() {
	case $1 in
	novix) echo "systemctl --user restart qswitchd" ;;
	macmini) echo "launchctl kickstart -k gui/$(id -u)/com.qswitch" ;;
	esac
}
sha_cmd() { case $1 in novix) echo "sha256sum" ;; macmini) echo "shasum -a 256" ;; esac; }

# on <target> <command>: runs a shell command on the target.
on() {
	local t=$1 host
	shift
	host=$(ssh_host "$t")
	if [ -n "$host" ]; then
		ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" "$*"
	else
		bash -c "$*"
	fi
}

# put <target> <local file> <name>: copies a file to ~/.local/bin/<name>.
put() {
	local t=$1 src=$2 name=$3 host
	host=$(ssh_host "$t")
	if [ -n "$host" ]; then
		scp -q "$src" "$host:.local/bin/$name"
	else
		cp "$src" "$HOME/.local/bin/$name"
	fi
}

health() { on "$1" "curl -s -m 3 127.0.0.1:7432/api/health" 2>/dev/null || true; }
version_in() { sed -n 's/.*"version":"\([^"]*\)".*/\1/p' <<<"$1"; }

# wait_version <target> <commit> <seconds>: polls health until it reports a
# version that is a prefix of the commit.
wait_version() {
	local t=$1 want=$2 limit=$3 i v
	for ((i = 0; i < limit; i++)); do
		v=$(version_in "$(health "$t")")
		if [ -n "$v" ] && [[ $want == "$v"* ]]; then
			echo "$v"
			return 0
		fi
		sleep 1
	done
	return 1
}

# latest_probe <target>: the newest quota observation the daemon shows, as a
# Unix time, 0 when the overview cannot be read.
latest_probe() {
	on "$1" "curl -s -m 5 127.0.0.1:7432/api/overview" 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(0)
    raise SystemExit
print(max([a.get("last_probe_at") or 0 for t in d.get("tools", []) for a in t.get("accounts", [])] or [0]))'
}

# Build from a fresh clone, never from a working copy or a worktree: a
# working copy may be stale or dirty, and Go embeds no revision in a
# worktree. On 11-03 two builds from the shared checkout shipped without
# later fixes, and on 11-04 an output written into the clone made the next
# build report itself modified.
here=$(cd "$(dirname "$0")/.." && pwd)
url=$(git -C "$here" remote get-url origin)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/qswitch-deploy.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
git clone -q "$url" "$tmp/repo"
rev=$(git -C "$tmp/repo" rev-parse --verify "$ref^{commit}") || die "unknown ref $ref"
git -C "$tmp/repo" checkout -q --detach "$rev"
short=${rev:0:12}
echo "deploying $short: $(git -C "$tmp/repo" log -1 --format=%s)"

# The Go mise manages is the one the daemons have been built with; the shell
# may carry a GOROOT and GOBIN for it that would mismatch another go.
go_bin=$HOME/.local/share/mise/installs/go/latest/bin/go
[ -x "$go_bin" ] || go_bin=$(command -v go || true)
[ -n "$go_bin" ] && [ -x "$go_bin" ] || die "go not found"
unset GOROOT GOBIN GOFLAGS

for t in "${targets[@]}"; do
	mkdir -p "$tmp/out/$t"
	for name in qswitch qswitchd; do
		(cd "$tmp/repo" && GOOS=$(goos "$t") GOARCH=$(goarch "$t") CGO_ENABLED=0 "$go_bin" build -trimpath -o "$tmp/out/$t/$name" "./cmd/$name")
		info=$("$go_bin" version -m "$tmp/out/$t/$name")
		if ! { grep -q "vcs.revision=$rev" <<<"$info" && grep -q "vcs.modified=false" <<<"$info"; }; then
			die "$name for $t is not a clean build of $rev"
		fi
	done
done

failed=0
for t in "${targets[@]}"; do
	old=$(version_in "$(health "$t")")
	[ -n "$old" ] || die "$t: qswitchd health does not answer; deploy by hand"
	if [[ $rev == "$old"* ]]; then
		echo "$t: already on $old"
		continue
	fi
	# Replacing the binaries during a switch would restart sessions with the
	# wrong code or leave the switch half done.
	if on "$t" "~/.local/bin/qswitch status" | grep -q 'queued_since=[^-]'; then
		die "$t: a switch is queued; try again when it is done"
	fi
	if on "$t" "test -e ~/.qswitch/managed-claude/pending.json"; then
		die "$t: a managed Claude rotation is in progress; try again when it is done"
	fi
	for name in qswitch qswitchd; do
		put "$t" "$tmp/out/$t/$name" "$name.next"
		want=$(shasum -a 256 "$tmp/out/$t/$name" | cut -d' ' -f1)
		got=$(on "$t" "$(sha_cmd "$t") ~/.local/bin/$name.next | cut -d' ' -f1")
		[ "$want" = "$got" ] || die "$t: $name.next does not match the build"
	done
	on "$t" "chmod 755 ~/.local/bin/qswitch.next ~/.local/bin/qswitchd.next"
	staged=$(on "$t" "~/.local/bin/qswitch.next version | head -1")
	[[ $rev == "$staged"* ]] || die "$t: staged qswitch reports $staged, want $short"
	staged=$(on "$t" "~/.local/bin/qswitchd.next --version | head -1")
	[[ $rev == "$staged"* ]] || die "$t: staged qswitchd reports $staged, want $short"
	# mv renames over the running binaries, which is what makes the managed
	# sessions' runners notice the new file.
	restarted=$(date +%s)
	on "$t" "cd ~/.local/bin && cp -p qswitch qswitch.prev-$old && cp -p qswitchd qswitchd.prev-$old && mv qswitch.next qswitch && mv qswitchd.next qswitchd && $(restart_cmd "$t")"
	# launchd kills the first spawn of a new binary (code signature) and
	# starts it again about 10 s later, so the wait covers that.
	if now=$(wait_version "$t" "$rev" 60); then
		echo "$t: qswitchd $old -> $now"
	else
		echo "$t: qswitchd did not come up on $short within 60 s; rolling back to $old" >&2
		on "$t" "cd ~/.local/bin && mv qswitch.prev-$old qswitch && mv qswitchd.prev-$old qswitchd && $(restart_cmd "$t")"
		if wait_version "$t" "$old" 60 >/dev/null; then
			echo "$t: back on $old" >&2
		else
			echo "$t: health does not report $old either; check the daemon" >&2
		fi
		exit 1
	fi
	# The first probe after the restart shows the daemon doing its real work:
	# the vault key, the provider request and the database. The backups go
	# only then. Probes are spaced by the accounts' intervals, so this can
	# take a while.
	echo "$t: waiting for the first quota probe"
	waited=0
	until [ "$(latest_probe "$t")" -gt "$restarted" ]; do
		if [ $waited -ge 900 ]; then
			echo "$t: no quota probe in 15 minutes; backups kept as ~/.local/bin/{qswitch,qswitchd}.prev-$old; check journal/log" >&2
			failed=1
			continue 2
		fi
		sleep 15
		waited=$((waited + 15))
	done
	on "$t" "rm -f ~/.local/bin/qswitch.prev-$old ~/.local/bin/qswitchd.prev-$old"
	echo "$t: first probe seen, backups removed"
	if [ "$(goos "$t")" = linux ]; then
		stale=$(on "$t" 'for p in $(pgrep -f "qswitch run claude"); do readlink /proc/$p/exe; done 2>/dev/null' | grep -c deleted || true)
		if [ "$stale" -gt 0 ]; then
			echo "$t: $stale managed session runner(s) still on the old binary; they upgrade when idle"
		else
			echo "$t: every managed session runner is on the new binary"
		fi
	fi
done
exit $failed
