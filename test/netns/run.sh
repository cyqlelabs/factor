#!/bin/sh
# The P2 checkpoint of docs/swarm.md: agents in separate network namespaces,
# each home network behind its own NAT with UDP dropped, pair through a real
# `factor rendezvous` on the "internet" between them and complete tasks
# through it.
#
#   wan        10.99.1.1  the rendezvous
#   r1         NAT for lan 1: desk 192.168.1.2, lap 192.168.1.3
#   r2         NAT for lan 2: home 192.168.1.3 (the same address as lap)
#
# Run it as root, with each one-way WAN delay to measure at; results land in
# $NETNS_OUT (default /tmp/factor-netns):
#
#   make build && go test -c -tags netns -o /tmp/swarm.test ./internal/swarm
#   pkexec --keep-cwd /usr/bin/env NETNS_OUT=/tmp/factor-netns /bin/sh test/netns/run.sh ./factor /tmp/swarm.test 0 20
set -eu
BIN=$(realpath "$1") TEST=$(realpath "$2")
shift 2
NS="fx-wan fx-r1 fx-r2 fx-desk fx-lap fx-home"
OUT=${NETNS_OUT:-/tmp/factor-netns}

cleanup() {
  for n in $NS; do
    ip netns pids "$n" 2>/dev/null | xargs -r kill 2>/dev/null || true
    ip netns del "$n" 2>/dev/null || true
  done
}
trap cleanup EXIT

veth() { # veth NS1 IF1 NS2 IF2
  ip link add "$2" netns "$1" type veth peer name "$4" netns "$3"
  ip -n "$1" link set "$2" up
  ip -n "$3" link set "$4" up
}

router() { # router NS WAN-ADDR
  ip -n "$1" addr add "$2/24" dev wan
  ip -n "$1" route add default via "${2%.*}.1"
  ip netns exec "$1" sysctl -qw net.ipv4.ip_forward=1
  ip netns exec "$1" nft -f - <<'EOF'
table ip nat { chain post { type nat hook postrouting priority srcnat; oifname "wan" masquerade; } }
table ip filter {
  chain fwd {
    type filter hook forward priority 0; policy accept;
    meta l4proto udp drop
    iifname "wan" ct state established,related accept
    iifname "wan" drop
  }
}
EOF
  ip -n "$1" link add lan type bridge
  ip -n "$1" addr add 192.168.1.1/24 dev lan
  ip -n "$1" link set lan up
}

host() { # host NS ROUTER ADDR
  veth "$1" eth0 "$2" "p-$1"
  ip -n "$2" link set "p-$1" master lan
  ip -n "$1" addr add "$3/24" dev eth0
  ip -n "$1" route add default via 192.168.1.1
}

delay() { # delay MS: one way, on every WAN link in both directions
  ip netns exec fx-wan tc qdisc replace dev w1 root netem delay "${1}ms"
  ip netns exec fx-wan tc qdisc replace dev w2 root netem delay "${1}ms"
  ip netns exec fx-r1 tc qdisc replace dev wan root netem delay "${1}ms"
  ip netns exec fx-r2 tc qdisc replace dev wan root netem delay "${1}ms"
}

# setup builds the networks. It runs under set -e, so a step that fails
# stops the script instead of reading later as an agent timing out.
setup() { # setup DELAY
  cleanup
  for n in $NS; do ip netns add "$n"; ip -n "$n" link set lo up; done
  veth fx-wan w1 fx-r1 wan
  veth fx-wan w2 fx-r2 wan
  ip -n fx-wan addr add 10.99.1.1/24 dev w1
  ip -n fx-wan addr add 10.99.2.1/24 dev w2
  ip netns exec fx-wan sysctl -qw net.ipv4.ip_forward=1
  router fx-r1 10.99.1.2
  router fx-r2 10.99.2.2
  host fx-desk fx-r1 192.168.1.2
  host fx-lap fx-r1 192.168.1.3
  host fx-home fx-r2 192.168.1.3
  delay "$1"
}

agents() { # agents DELAY
  dir="$OUT/delay-${1}ms"
  rm -rf "$dir" && mkdir -p "$dir/shared"
  ip netns exec fx-wan "$BIN" rendezvous --listen 10.99.1.1:8740 --data "$dir/rendezvous" --init \
    >"$dir/rendezvous.log" 2>&1 &
  sleep 1
  ip netns exec fx-desk ping -c 3 -q 10.99.1.1 | tail -1 >"$dir/rtt.txt"
  pids=""
  for role in desk home lap; do
    ip netns exec "fx-$role" env NETNS_ROLE=$role NETNS_DIR="$dir/shared" NETNS_RENDEZVOUS=ws://10.99.1.1:8740 \
      "$TEST" -test.run '^TestNetnsAgent$' -test.v -test.timeout 180s >"$dir/$role.log" 2>&1 &
    pids="$pids $!"
  done
  rc=0
  for p in $pids; do wait "$p" || rc=1; done
  echo "==> one-way WAN delay ${1} ms: $( [ $rc = 0 ] && echo pass || echo FAIL ), $(cat "$dir/rtt.txt")"
  [ -f "$dir/shared/result.json" ] && cat "$dir/shared/result.json"
  return $rc
}

[ $# -gt 0 ] || set -- 0
status=0
for d in "$@"; do
  setup "$d"
  agents "$d" || status=1
done
[ -n "${PKEXEC_UID:-}" ] && chown -R "$PKEXEC_UID" "$OUT"
exit $status
