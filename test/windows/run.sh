#!/usr/bin/env bash
# Runs the Go suite against real Windows in the VirtualBox guest.
#
# Two channels, because Windows has two kinds of session. Plain ssh lands in a
# non-interactive window station, which is fine for everything that is only
# Go, the filesystem, the registry and processes. The desktop, tray and
# grid-vision tests need a window station with a desktop behind it, and the
# only way back into one is a task registered against the logged-on session.
# `test` uses that channel and is therefore the honest gate; `fast` uses ssh
# and is the one to iterate against.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
VMCTL="$HERE/vmctl.sh"
GUEST_SRC=${FACTOR_WIN_SRC:-C:/factor-test/src}
GUEST_SRC_WIN=${GUEST_SRC//\//\\}

die() { echo "run: $*" >&2; exit 1; }

# sync ships the working tree, not HEAD: the point of this harness is to test
# the change in front of you. Tracked files plus untracked-and-not-ignored
# ones, so a test file you have written but not committed still crosses.
sync_tree() {
  echo "==> syncing $REPO -> $GUEST_SRC"
  "$VMCTL" ssh "if exist $GUEST_SRC_WIN rmdir /s /q $GUEST_SRC_WIN" || true
  "$VMCTL" ssh "mkdir $GUEST_SRC_WIN"
  git -C "$REPO" ls-files -c -o --exclude-standard -z \
    | tar -czf - -C "$REPO" --null -T - \
    | "$VMCTL" ssh "tar -xzf - -C $GUEST_SRC"
}

# run_interactive hands one script to the logged-on session and waits for the
# exit code to appear, streaming what has been written so far. A suite that
# runs for ten minutes with nothing on the terminal is indistinguishable from
# one that hung, and this harness has enough places to hang already.
run_interactive() {
  local script=$1 timeout=${2:-3600}
  local tmp; tmp=$(mktemp)

  "$VMCTL" push "$script" 'C:/factor-test/job.ps1'
  # job.out goes too, not just job.rc: the snapshot carries whatever the last
  # job printed, and the host starts reading before the task truncates it, so
  # the previous run's output arrives as though it were this one's.
  "$VMCTL" ssh 'del /q C:\factor-test\job.rc C:\factor-test\job.out 2>nul & schtasks /run /tn FactorTests' >/dev/null

  local seen=0 deadline=$(( $(date +%s) + timeout )) rc=
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if "$VMCTL" pull 'C:/factor-test/job.out' "$tmp" 2>/dev/null; then
      local lines; lines=$(wc -l < "$tmp")
      if [ "$lines" -gt "$seen" ]; then
        tail -n +$((seen + 1)) "$tmp"
        seen=$lines
      fi
    fi
    rc=$("$VMCTL" ssh 'type C:\factor-test\job.rc' 2>/dev/null | tr -d '\r' | grep -oE '^[0-9]+$' | head -1) || rc=
    if [ -n "$rc" ]; then break; fi
    sleep 5
  done
  [ -n "${rc:-}" ] || { "$VMCTL" shot /tmp/factor-win-hang.png; rm -f "$tmp"; die "no exit code within ${timeout}s; framebuffer saved to /tmp/factor-win-hang.png"; }

  "$VMCTL" pull 'C:/factor-test/job.out' "$tmp" 2>/dev/null || true
  local lines; lines=$(wc -l < "$tmp")
  if [ "$lines" -gt "$seen" ]; then tail -n +$((seen + 1)) "$tmp"; fi
  rm -f "$tmp"
  return "$rc"
}

# withCount ensures -count=1 is on a `go test` invocation. Go caches test
# results and replays them, and what it keys on does not include the machine's
# configuration - so a skip recorded when the guest had no Developer Mode, no
# Python or no browser is replayed verbatim onto a guest that now has all
# three. The gate then reports a pass over tests that never ran, which is the
# one failure this harness exists to make impossible.
withCount() {
  case "$1" in
    test*) case "$1" in *-count=*) echo "$1" ;; *) echo "test -count=1 ${1#test }" ;; esac ;;
    *) echo "$1" ;;
  esac
}

# job writes the script the guest will run. GOFLAGS carries no -race by
# default: the detector needs cgo, and cgo needs the C toolchain bootstrap
# installs as a best effort.
job() {
  local goargs=$1 cgo=${2:-0}
  cat <<EOF
\$ErrorActionPreference = "Continue"
\$env:CGO_ENABLED = "$cgo"
# Prepended rather than relied upon: the task's environment block is built
# when it starts, and a machine PATH written after this session logged on has
# been seen not to reach it. Naming the directories costs nothing and removes
# a failure that reads as "go is not installed".
\$env:PATH = "C:\\go\\bin;C:\\w64devkit\\bin;" + \$env:PATH
Set-Location "$GUEST_SRC"
Write-Host "go version: \$(& go version)"
Write-Host "session:    \$([Environment]::UserName) on \$([Environment]::MachineName)"
& go $goargs
exit \$LASTEXITCODE
EOF
}

# The guest carries no Docker and no drivable Chromium, so the packages whose
# tests need them are named here and left out of the default gate. Python and
# SoX are provisioned by setup, so the voice tier is in the gate. Named, not silently skipped: a run that quietly drops
# coverage reads as "everything passed", and a gate nobody can ever get green
# stops being read at all. FACTOR_WIN_ALL=1 runs the whole tree anyway.
WIN_EXCLUDE=${FACTOR_WIN_EXCLUDE:-"internal/browser"}

win_packages() {
  if [ -n "${FACTOR_WIN_ALL:-}" ]; then echo "./..."; return; fi
  local pat="" p
  for p in $WIN_EXCLUDE; do pat="${pat:+$pat|}/${p}\$"; done
  GOOS=windows go list ./... 2>/dev/null | grep -Ev "($pat)" | tr '\n' ' '
}

announce_exclusions() {
  [ -n "${FACTOR_WIN_ALL:-}" ] && { echo "==> including every package (FACTOR_WIN_ALL)"; return; }
  echo "==> excluding $WIN_EXCLUDE - needs a drivable Chromium, which this guest does not carry; FACTOR_WIN_ALL=1 to run it"
}

cmd_sync() { sync_tree; }

# prime warms the module and build caches, then the machine is snapshotted
# with them in place. Without this every run restores a guest with an empty
# cache and spends its first minutes downloading modules it had last time.
cmd_prime() {
  sync_tree
  echo "==> warming the module and build caches"
  "$VMCTL" ssh "cd /d $GUEST_SRC_WIN && go mod download && go build ./... && go vet ./..." || true
}

cmd_fast() {
  sync_tree
  local args=$*
  if [ -z "$args" ]; then announce_exclusions; args="test $(win_packages)"; fi
  args=$(withCount "$args")
  echo "==> go $args (non-interactive session)"
  "$VMCTL" ssh "cd /d $GUEST_SRC_WIN && go $args"
}

cmd_test() {
  sync_tree
  "$VMCTL" wait-session 300
  local args=$*
  if [ -z "$args" ]; then announce_exclusions; args="test $(win_packages)"; fi
  args=$(withCount "$args")
  local tmp; tmp=$(mktemp)
  job "$args" 0 > "$tmp"
  echo "==> go $args (interactive session)"
  run_interactive "$tmp" 3600
}

cmd_race() {
  sync_tree
  "$VMCTL" wait-session 300
  local tmp; tmp=$(mktemp)
  job "test -race -count=1 ./..." 1 > "$tmp"
  echo "==> go test -race ./... (interactive session, cgo on)"
  run_interactive "$tmp" 5400
}

# setup is the whole first run: bootstrap the guest, let autologon take
# effect, warm the caches, then capture the snapshot every later run starts
# from. It ends with the VM off and a clean snapshot on disk.
cmd_setup() {
  "$VMCTL" bootstrap
  echo "==> rebooting for autologon"
  "$VMCTL" ssh "shutdown /r /t 0" || true
  sleep 20
  "$VMCTL" wait-ssh 420
  "$VMCTL" wait-session 300
  # Python and SoX, so the local voice tier is exercised rather than skipped.
  # Nine of its tests used to skip silently on a guest without them, which
  # read as a green package that had never run the speech path at all.
  echo "==> provisioning the local voice tier"
  "$VMCTL" gcjob "$HERE/provision.ps1" 1800
  cmd_prime
  "$VMCTL" ssh "shutdown /s /t 0" || true
  echo "==> waiting for the guest to power down"
  for _ in $(seq 1 60); do VBoxManage list runningvms | grep -q "\"${FACTOR_WIN_VM:-win10}\"" || break; sleep 5; done
  VBoxManage list runningvms | grep -q "\"${FACTOR_WIN_VM:-win10}\"" && "$VMCTL" down
  sleep 3
  "$VMCTL" snapshot clean
  echo "==> setup complete; 'run.sh test' is now repeatable"
}

# A run starts from the snapshot every time. The suite writes to the HKCU Run
# key, drops %USERPROFILE%\.factor and renames the running binary, so a guest
# that carried state forward would be grading the previous run's leftovers.
cmd_ci() {
  "$VMCTL" restore
  "$VMCTL" up
  "$VMCTL" wait-ssh 420
  local rc=0
  cmd_test "${@}" || rc=$?
  "$VMCTL" down
  return "$rc"
}

case "${1:-}" in
  setup) shift; cmd_setup ;;
  sync)  shift; cmd_sync ;;
  prime) shift; cmd_prime ;;
  fast)  shift; cmd_fast "$@" ;;
  test)  shift; cmd_test "$@" ;;
  race)  shift; cmd_race ;;
  ci)    shift; cmd_ci "$@" ;;
  *) die "usage: run.sh {setup|sync|prime|fast [go args]|test [go args]|race|ci [go args]}" ;;
esac
