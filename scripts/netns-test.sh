#!/bin/bash
# Run two P2PV clients in separate network namespaces on one Linux host.
#
# Testing a NAT-traversing VPN normally needs two machines behind two different
# NATs. Namespaces stand in for that: each client gets its own routing table and
# its own tun interface, so packets between them have to leave one namespace and
# come back into the other -- exactly the path that matters.
#
# The two phases are the point of the script:
#
#   phase 1, ip_forward=0 -- the host refuses to route between the namespaces,
#            so hole punching cannot succeed and traffic must go via the relay.
#   phase 2, ip_forward=1 -- a direct path becomes possible, and the clients
#            should upgrade to it on their own, without dropping the session.
#
# Usage: sudo ./netns-test.sh [server-port]
set -u

PORT="${1:-27241}"
NS_A=p2pv-a
NS_B=p2pv-b
CFG_A=/tmp/p2pv-cfg-a
CFG_B=/tmp/p2pv-cfg-b
LOG_A=/tmp/p2pv-a.log
LOG_B=/tmp/p2pv-b.log
# The network name carries the run id, not just the password. The first client
# to use a name fixes that network's password on the server, permanently -- so a
# fixed name with a per-run password makes every run after the first fail with
# "wrong network password". Varying both keeps each run self-contained.
RUN_ID=$$
NETWORK=netns-test-$RUN_ID
PASSWORD=test-password-$RUN_ID

FORWARD_WAS=$(sysctl -n net.ipv4.ip_forward)

cleanup() {
  echo
  echo "--- cleaning up ---"
  for ns in $NS_A $NS_B; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null
  done
  sleep 1
  ip netns del $NS_A 2>/dev/null
  ip netns del $NS_B 2>/dev/null
  ip link del veth-a 2>/dev/null
  ip link del veth-b 2>/dev/null
  rm -rf "$CFG_A" "$CFG_B"
  sysctl -qw net.ipv4.ip_forward="$FORWARD_WAS"
  echo "ip_forward restored to $FORWARD_WAS"
}
trap cleanup EXIT

setup_ns() {
  local ns=$1 host_if=$2 ns_if=$3 host_ip=$4 ns_ip=$5
  ip netns add "$ns"
  ip link add "$host_if" type veth peer name "$ns_if"
  ip link set "$ns_if" netns "$ns"
  ip addr add "$host_ip/24" dev "$host_if"
  ip link set "$host_if" up
  ip netns exec "$ns" ip addr add "$ns_ip/24" dev "$ns_if"
  ip netns exec "$ns" ip link set "$ns_if" up
  ip netns exec "$ns" ip link set lo up
  ip netns exec "$ns" ip route add default via "$host_ip"
}

start_client() {
  local ns=$1 cfg=$2 server=$3 log=$4
  rm -rf "$cfg"
  mkdir -p "$cfg"
  # -lan-discovery is on here so the test covers the multicast path too. It is
  # off by default in real use, because it routes all multicast to the tunnel.
  ip netns exec "$ns" env XDG_CONFIG_HOME="$cfg" \
    p2pv join "$NETWORK" "$PASSWORD" -server "$server" -iface p2pv0 -v -lan-discovery \
    > "$log" 2>&1 &
}

vip_from_log() {
  grep -o 'virtual IP [0-9.]*' "$1" 2>/dev/null | tail -1 | awk '{print $3}'
}

path_state() {
  # Reports how the last connection to a peer was established.
  if grep -q 'direct path to' "$1" 2>/dev/null; then echo "direct"; else echo "relay"; fi
}

echo "=== setup ==="
cleanup 2>/dev/null
trap cleanup EXIT
setup_ns $NS_A veth-a veth-a-ns 10.200.1.1 10.200.1.2
setup_ns $NS_B veth-b veth-b-ns 10.200.2.1 10.200.2.2
sysctl -qw net.ipv4.ip_forward=0
echo "two namespaces up, ip_forward=0 (no route between them)"

echo
echo "=== phase 1: relay path ==="
start_client $NS_A "$CFG_A" "10.200.1.1:$PORT" "$LOG_A"
start_client $NS_B "$CFG_B" "10.200.2.1:$PORT" "$LOG_B"
sleep 8

VIP_A=$(vip_from_log "$LOG_A")
VIP_B=$(vip_from_log "$LOG_B")
echo "client A: ${VIP_A:-FAILED}"
echo "client B: ${VIP_B:-FAILED}"

if [ -z "$VIP_A" ] || [ -z "$VIP_B" ]; then
  echo "FAIL: a client did not get a virtual IP"
  echo "--- log A ---"; tail -20 "$LOG_A"
  echo "--- log B ---"; tail -20 "$LOG_B"
  exit 1
fi

echo "path A->B: $(path_state "$LOG_A") (expected relay)"
echo
echo "ping A -> B over the relay:"
ip netns exec $NS_A ping -c 3 -W 3 "$VIP_B"
RELAY_PING=$?

echo
echo "=== phase 2: direct path ==="
echo "enabling ip_forward so the namespaces can reach each other"
sysctl -qw net.ipv4.ip_forward=1
# Punch retries run every 2s; give them a few rounds.
sleep 10

echo "path A->B: $(path_state "$LOG_A") (expected direct)"
echo
echo "ping A -> B, same session:"
ip netns exec $NS_A ping -c 3 -W 3 "$VIP_B"
DIRECT_PING=$?

echo
echo "=== TCP through the tunnel ==="
# A real game is TCP, so prove a stream works, not just ICMP.
ip netns exec $NS_B p2pv-probe tcp-listen 25565 > /tmp/p2pv-tcp.out 2>&1 &
sleep 1
ip netns exec $NS_A p2pv-probe tcp-send "$VIP_B:25565" "hello from A" 2>/dev/null
sleep 1
TCP_OUT=$(cat /tmp/p2pv-tcp.out 2>/dev/null)
echo "received over TCP: '${TCP_OUT:-nothing}'"

echo
echo "=== broadcast fan-out ==="
# Broadcast is what a layer 3 tunnel does not carry on its own, so this checks
# the software fan-out rather than the kernel.
BCAST=$(echo "$VIP_B" | awk -F. '{print $1"."$2"."$3".255"}')
ip netns exec $NS_B p2pv-probe udp-listen 4445 > /tmp/p2pv-bcast.out 2>&1 &
sleep 1
ip netns exec $NS_A p2pv-probe udp-send "$BCAST:4445" '[MOTD]P2PV world[/MOTD][AD]25565[/AD]' 2>/dev/null
sleep 2
BCAST_OUT=$(cat /tmp/p2pv-bcast.out 2>/dev/null)
echo "received over broadcast: '${BCAST_OUT:-nothing}'"

echo
echo "=== multicast fan-out (Minecraft LAN discovery) ==="
# The exact address and port Minecraft announces open worlds on.
ip netns exec $NS_B p2pv-probe mc-listen 224.0.2.60:4445 > /tmp/p2pv-mc.out 2>&1 &
sleep 1
ip netns exec $NS_A p2pv-probe mc-send 224.0.2.60:4445 '[MOTD]P2PV world[/MOTD][AD]25565[/AD]' 2>/dev/null
sleep 2
MC_OUT=$(cat /tmp/p2pv-mc.out 2>/dev/null)
echo "received over multicast: '${MC_OUT:-nothing}'"

echo
echo "=== result ==="
[ $RELAY_PING -eq 0 ] && echo "PASS relay ping" || echo "FAIL relay ping"
[ $DIRECT_PING -eq 0 ] && echo "PASS direct ping" || echo "FAIL direct ping"
[ "$(path_state "$LOG_A")" = "direct" ] && echo "PASS upgraded to a direct path" || echo "FAIL still relaying"
[ -n "$TCP_OUT" ] && echo "PASS TCP stream" || echo "FAIL TCP stream"
[ -n "$BCAST_OUT" ] && echo "PASS broadcast fan-out" || echo "FAIL broadcast fan-out"
[ -n "$MC_OUT" ] && echo "PASS multicast fan-out" || echo "FAIL multicast fan-out"

echo
echo "--- client A log ---"
tail -15 "$LOG_A"
