#!/bin/bash
# Diagnose why multicast does not arrive over the tunnel, and test the fix.
#
# The fan-out itself is known to work: a datagram sent to the subnet broadcast
# address reaches the other namespace. Multicast differs on the receiving side.
# A process joining 224.0.2.60 picks an interface to join on, and the kernel
# picks that interface from the routing table. With no route for 224.0.0.0/4 the
# join lands on whatever interface holds the default route -- not the tunnel --
# so the packet arrives on p2pv0 and is discarded.
#
# This adds a route for the multicast range to the tunnel and re-tests.
set -u

NS_A=p2pv-a
NS_B=p2pv-b
CFG_A=/tmp/p2pv-cfg-a
CFG_B=/tmp/p2pv-cfg-b
NETWORK=mc-test
PASSWORD=mc-test-$$
PORT=27241

FORWARD_WAS=$(sysctl -n net.ipv4.ip_forward)

cleanup() {
  for ns in $NS_A $NS_B; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; done
  sleep 1
  ip netns del $NS_A 2>/dev/null; ip netns del $NS_B 2>/dev/null
  ip link del veth-a 2>/dev/null; ip link del veth-b 2>/dev/null
  rm -rf "$CFG_A" "$CFG_B"
  sysctl -qw net.ipv4.ip_forward="$FORWARD_WAS"
}
trap cleanup EXIT
cleanup 2>/dev/null
trap cleanup EXIT

setup_ns() {
  local ns=$1 host_if=$2 ns_if=$3 host_ip=$4 ns_ip=$5
  ip netns add "$ns"
  ip link add "$host_if" type veth peer name "$ns_if"
  ip link set "$ns_if" netns "$ns"
  ip addr add "$host_ip/24" dev "$host_if"; ip link set "$host_if" up
  ip netns exec "$ns" ip addr add "$ns_ip/24" dev "$ns_if"
  ip netns exec "$ns" ip link set "$ns_if" up
  ip netns exec "$ns" ip link set lo up
  ip netns exec "$ns" ip route add default via "$host_ip"
}

setup_ns $NS_A veth-a veth-a-ns 10.200.1.1 10.200.1.2
setup_ns $NS_B veth-b veth-b-ns 10.200.2.1 10.200.2.2
sysctl -qw net.ipv4.ip_forward=1

rm -rf "$CFG_A" "$CFG_B"; mkdir -p "$CFG_A" "$CFG_B"
ip netns exec $NS_A env XDG_CONFIG_HOME="$CFG_A" p2pv join "$NETWORK" "$PASSWORD" -server "10.200.1.1:$PORT" -iface p2pv0 > /tmp/mc-a.log 2>&1 &
ip netns exec $NS_B env XDG_CONFIG_HOME="$CFG_B" p2pv join "$NETWORK" "$PASSWORD" -server "10.200.2.1:$PORT" -iface p2pv0 > /tmp/mc-b.log 2>&1 &
sleep 8

VIP_A=$(grep -o 'virtual IP [0-9.]*' /tmp/mc-a.log | tail -1 | awk '{print $3}')
VIP_B=$(grep -o 'virtual IP [0-9.]*' /tmp/mc-b.log | tail -1 | awk '{print $3}')
echo "A=$VIP_A  B=$VIP_B"

echo
echo "=== routing state in namespace B, before ==="
ip netns exec $NS_B ip route show | sed 's/^/  /'
echo "  multicast route lookup for 224.0.2.60:"
ip netns exec $NS_B ip route get 224.0.2.60 2>&1 | sed 's/^/    /'

echo
echo "=== test 1: no multicast route (current behaviour) ==="
ip netns exec $NS_B p2pv-probe mc-listen 224.0.2.60:4445 > /tmp/mc1.out 2>&1 &
sleep 1
ip netns exec $NS_A p2pv-probe mc-send 224.0.2.60:4445 'announce-1' 2>/dev/null
sleep 2
echo "  result: '$(cat /tmp/mc1.out 2>/dev/null || echo nothing)'"

echo
echo "=== test 2: with a multicast route on the tunnel ==="
ip netns exec $NS_B ip route add 224.0.0.0/4 dev p2pv0
ip netns exec $NS_A ip route add 224.0.0.0/4 dev p2pv0
echo "  multicast route lookup for 224.0.2.60 now:"
ip netns exec $NS_B ip route get 224.0.2.60 2>&1 | sed 's/^/    /'

ip netns exec $NS_B p2pv-probe mc-listen 224.0.2.60:4445 > /tmp/mc2.out 2>&1 &
sleep 1
ip netns exec $NS_A p2pv-probe mc-send 224.0.2.60:4445 'announce-2' 2>/dev/null
sleep 2
echo "  result: '$(cat /tmp/mc2.out 2>/dev/null || echo nothing)'"

echo
echo "=== what the sender put on the wire ==="
# Confirm the tunnel carried the datagram either way, so we know which side is at fault.
ip netns exec $NS_B timeout 4 tcpdump -n -i p2pv0 -c 3 'udp port 4445' 2>&1 | sed 's/^/  /' &
sleep 1
ip netns exec $NS_A p2pv-probe mc-send 224.0.2.60:4445 'announce-3' 2>/dev/null
wait
