#!/usr/bin/env bash
# Lifecycle for the Windows test VM. VBoxManage owns only what happens to the
# machine — power, snapshots, the one bootstrap that predates any network
# path in. Everything after that rides ssh, which is the only transport here
# that reports an exit code the shell can trust.
set -euo pipefail

VM=${FACTOR_WIN_VM:-win10}
SSH_PORT=${FACTOR_WIN_SSH_PORT:-2222}
SSH_USER=${FACTOR_WIN_USER:-}
SSH_KEY=${FACTOR_WIN_KEY:-$HOME/.ssh/factor-win10}
SNAPSHOT=${FACTOR_WIN_SNAPSHOT:-clean}
CREDFILE=${FACTOR_WIN_CREDFILE:-$HOME/.config/factor-win10.pass}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# The VM is restored from a snapshot between runs, and a re-bootstrap mints a
# fresh host key. Pinning it would mean a known_hosts edit every time the
# thing it protects against cannot happen: this is a loopback-bound VM on the
# developer's own machine.
COMMON_OPTS=(-i "$SSH_KEY"
             -o StrictHostKeyChecking=no
             -o UserKnownHostsFile=/dev/null
             -o GlobalKnownHostsFile=/dev/null
             -o LogLevel=ERROR
             -o ConnectTimeout=5)
# scp spells the port -P; ssh spells it -p, where scp reads it as "preserve
# times" and then treats the port number as a filename. One shared array for
# both is a silent, confusing failure rather than a loud one.
SSH_OPTS=(-p "$SSH_PORT" "${COMMON_OPTS[@]}")
SCP_OPTS=(-P "$SSH_PORT" "${COMMON_OPTS[@]}")

die() { echo "vmctl: $*" >&2; exit 1; }

# A blank-password account is passed as an explicit empty password rather than
# an empty file, which VBoxManage rejects. Windows refuses such an account for
# any non-console logon until LimitBlankPasswordUse is cleared — see the note
# in bootstrap.ps1 for why that is a one-time manual step on a fresh guest.
if [ -s "$CREDFILE" ]; then GC_PASS=(--password-file "$CREDFILE"); else GC_PASS=(--password ""); fi

GC() { VBoxManage guestcontrol "$VM" --username "$SSH_USER" "${GC_PASS[@]}" "$@"; }
GCRUN() { GC run --exe 'C:\Windows\System32\cmd.exe' --wait-stdout --wait-stderr -- /c "$1"; }

# gcjob runs a PowerShell script through the FactorInteractive task, using
# Guest Additions as the transport. This is the channel that exists before
# ssh does, and the only one that is both elevated and attached to a desktop:
# guestcontrol hands out a UAC-filtered token with Administrators marked deny
# only, so anything touching HKLM, a service or the firewall fails through it.
gcjob() {
  need_user
  local script=$1 timeout=${2:-1800}
  # Staged under the name the guest expects rather than renamed once there:
  # the guest-side move needed a backslash immediately before a command
  # substitution, which bash reads as an escaped dollar and never expands.
  local stage; stage=$(mktemp -d)
  cp "$script" "$stage/job.ps1"
  GC copyto --target-directory 'C:\factor-test\' "$stage/job.ps1" >/dev/null
  rm -rf "$stage"
  GCRUN "del /q C:\factor-test\job.rc C:\factor-test\job.out" >/dev/null 2>&1 || true
  GCRUN "schtasks /run /tn FactorInteractive" >/dev/null 2>&1

  local deadline=$(( $(date +%s) + timeout )) rc=
  while [ "$(date +%s)" -lt "$deadline" ]; do
    rc=$(GCRUN "type C:\factor-test\job.rc" 2>/dev/null | tr -d '\r' | grep -oE '^[0-9]+$' | head -1) || rc=
    if [ -n "$rc" ]; then break; fi
    sleep 5
  done
  GCRUN "type C:\factor-test\job.out" 2>/dev/null || true
  [ -n "${rc:-}" ] || die "the interactive job produced no exit code within ${timeout}s"
  return "$rc"
}

need_user() { [ -n "$SSH_USER" ] || die "set FACTOR_WIN_USER to the Windows account name"; }

running() { VBoxManage list runningvms | grep -q "\"$VM\""; }

# up starts the machine headless. Headless still gives the guest a virtual
# GPU and a full interactive session once autologon runs, which is what the
# desktop and tray tests need — it only means no window on this desktop.
up() {
  running && { echo "$VM already running"; return; }
  VBoxManage startvm "$VM" --type headless >/dev/null
  echo "$VM starting"
}

down() {
  running || { echo "$VM already off"; return; }
  VBoxManage controlvm "$VM" poweroff >/dev/null
  echo "$VM off"
}

# restore returns the disk to the snapshot every run starts from. Without it
# the second run is reading the first one's leftovers: the suite writes to the
# HKCU Run key, drops %USERPROFILE%\.factor, and renames the running binary.
restore() {
  running && VBoxManage controlvm "$VM" poweroff >/dev/null && sleep 2
  VBoxManage snapshot "$VM" restore "$SNAPSHOT" >/dev/null
  echo "restored $SNAPSHOT"
}

snap() {
  local name=${1:-$SNAPSHOT}
  running && die "power the VM off before snapshotting; a live snapshot carries the running state too"
  VBoxManage snapshot "$VM" take "$name" >/dev/null
  echo "took snapshot $name"
}

# wait_ssh polls the forwarded port rather than the guest properties: what
# matters is that a command can be run, not that the OS reached a run level.
wait_ssh() {
  need_user
  local deadline=$(( $(date +%s) + ${1:-300} ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if ssh "${SSH_OPTS[@]}" "$SSH_USER@127.0.0.1" "exit 0" 2>/dev/null; then
      echo "ssh up"; return 0
    fi
    sleep 5
  done
  die "no ssh on 127.0.0.1:$SSH_PORT within ${1:-300}s"
}

# wait_session waits for someone to be logged in. Autologon gets there on its
# own, but the interactive tests must not start before it does — a task with
# "run only when the user is logged on" silently does nothing otherwise.
wait_session() {
  local deadline=$(( $(date +%s) + ${1:-300} ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local n
    n=$(VBoxManage guestproperty get "$VM" /VirtualBox/GuestInfo/OS/LoggedInUsers 2>/dev/null | sed -n 's/^Value: //p')
    [ "${n:-0}" -ge 1 ] 2>/dev/null && { echo "session up ($n user)"; return 0; }
    sleep 5
  done
  die "no interactive session within ${1:-300}s; autologon did not complete"
}

sh_() { need_user; ssh "${SSH_OPTS[@]}" "$SSH_USER@127.0.0.1" "$@"; }

push() { need_user; scp "${SCP_OPTS[@]}" "$1" "$SSH_USER@127.0.0.1:$2"; }
pull() { need_user; scp "${SCP_OPTS[@]}" "$SSH_USER@127.0.0.1:$1" "$2"; }

# shot captures the framebuffer through the hypervisor. It needs nothing from
# the guest, which is exactly why it is here: when a run hangs, this is the
# one probe that still answers.
shot() {
  local out=${1:-/tmp/$VM-screen.png}
  VBoxManage controlvm "$VM" screenshotpng "$out"
  echo "$out"
}

# bootstrap is the one step that predates ssh, so it goes through Guest
# Additions. It is idempotent: re-running it repairs a VM rather than
# doubling anything up.
bootstrap() {
  need_user
  [ -f "$CREDFILE" ] || die "put the Windows password in $CREDFILE (chmod 600)"
  [ -f "$SSH_KEY" ] || { ssh-keygen -t ed25519 -N '' -C factor-win10 -f "$SSH_KEY" >/dev/null; echo "minted $SSH_KEY"; }
  running || up
  echo "waiting for Guest Additions..."
  local deadline=$(( $(date +%s) + 300 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    VBoxManage guestproperty get "$VM" /VirtualBox/GuestAdd/Version 2>/dev/null | grep -q '^Value:' && break
    sleep 5
  done

  # Windows PowerShell 5.1 reads a script without a BOM as Windows-1252, and a
  # UTF-8 em-dash lands there as a byte it accepts as a smart quote - which is
  # a string delimiter. One in a comment is harmless; one inside a string
  # silently unbalances every quote after it and the whole file fails to
  # parse. Cheaper to forbid the bytes than to debug that twice.
  local f
  for f in "$HERE"/*.ps1; do
    if LC_ALL=C grep -qP '[^\x00-\x7F]' "$f"; then
      die "$(basename "$f") contains non-ASCII bytes; PowerShell 5.1 will misparse it (see the note in vmctl.sh)"
    fi
  done

  local goversion
  goversion=$(awk '/^go /{print $2; exit}' "$HERE/../../go.mod")

  GC mkdir --parents 'C:\factor-test' >/dev/null 2>&1 || true
  GC copyto --target-directory 'C:\factor-test\' \
      "$HERE/bootstrap.ps1" "$HERE/interactive.ps1" "$HERE/register-task.ps1" \
      "$SSH_KEY.pub" "$CREDFILE"

  echo "running bootstrap.ps1 through the elevated task (OpenSSH, Go $goversion, cache exclusions)..."
  # The guest-side paths are assembled here rather than inside the heredoc: a
  # backslash immediately before a $ is an escape in both a double-quoted
  # string and an unquoted heredoc, so `C:\factor-test\$name` silently ships
  # the literal text instead of the value.
  local guest_pub guest_cred wrapper
  guest_pub='C:\factor-test\'$(basename "$SSH_KEY.pub")
  guest_cred='C:\factor-test\'$(basename "$CREDFILE")
  wrapper=$(mktemp /tmp/factor-bootstrap-job-XXXX.ps1)
  cat > "$wrapper" <<EOF
& 'C:\factor-test\bootstrap.ps1' -PubKeyFile '$guest_pub' -PassFile '$guest_cred' -User '$SSH_USER' -GoVersion '$goversion'
exit \$LASTEXITCODE
EOF
  gcjob "$wrapper" 2400
  rm -f "$wrapper"
}

case "${1:-}" in
  up)           up ;;
  down)         down ;;
  restore)      restore ;;
  snapshot)     shift; snap "${1:-}" ;;
  wait-ssh)     shift; wait_ssh "${1:-300}" ;;
  wait-session) shift; wait_session "${1:-300}" ;;
  ssh)          shift; sh_ "$@" ;;
  push)         shift; push "$1" "$2" ;;
  pull)         shift; pull "$1" "$2" ;;
  shot)         shift; shot "${1:-}" ;;
  bootstrap)    bootstrap ;;
  gcjob)        shift; gcjob "$1" "${2:-1800}" ;;
  status)
    echo "vm:       $VM ($(running && echo running || echo off))"
    echo "ssh:      $SSH_USER@127.0.0.1:$SSH_PORT key=$SSH_KEY"
    echo "snapshot: $SNAPSHOT"
    VBoxManage snapshot "$VM" list 2>&1 | head -5
    ;;
  *) die "usage: vmctl.sh {up|down|restore|snapshot [name]|wait-ssh|wait-session|ssh CMD|push SRC DST|pull SRC DST|shot [png]|bootstrap|gcjob FILE|status}" ;;
esac
