#!/bin/sh
# Makes the Linux kernel MACsec captures macsec_test.go checks against. Runs in
# a privileged container with iproute2 and tcpdump (Debian bookworm here):
#   docker run --rm --privileged -v $PWD/gen.sh:/gen.sh -v $PWD:/out IMAGE sh /gen.sh NAME CIPHER KEY ENCRYPT SEND_SCI LAYOUT
# Two ends (the root namespace and n1) joined by a veth pair, 02:00:00:00:00:a1
# and :b1, a MACsec link on each with static keys (SA 0; XPN: salt 0102…0c,
# SSCI 1 and 2), pings both ways; the wire (v0) goes to /out/NAME.pcap.
#   LAYOUT: port | clear (MACsec on v.100: the tag in clear) | enc (VLAN 100 over MACsec: the tag encrypted)
# XPN=N starts the packet numbers at N (xpnwrap: 4294967294). The captures:
#   gcm128 gcm-aes-128 K128 on on port     gcm256 gcm-aes-256 K256 on on port
#   xpn128 gcm-aes-xpn-128 K128 on on port xpn256 gcm-aes-xpn-256 K256 on on port
#   xpnwrap (XPN=4294967294) gcm-aes-xpn-128 K128 on on port
#   integ gcm-aes-128 K128 off on port     nosci gcm-aes-128 K128 on off port
#   clear gcm-aes-128 K128 on on clear     enctag gcm-aes-128 K128 on on enc
# with K128=000102…0f and K256=000102…1f.
set -e
name=$1 cipher=$2 key=$3 enc=$4 sci=$5 layout=$6
ip netns add n1
ip link add v0 address 02:00:00:00:00:a1 type veth peer name v1 address 02:00:00:00:00:b1 netns n1
side() { # side N CMD...
  n=$1; shift; if [ $n = 0 ]; then "$@"; else ip netns exec n1 "$@"; fi; }
for s in 0 1; do
  mine=$([ $s = 0 ] && echo 02:00:00:00:00:a1 || echo 02:00:00:00:00:b1)
  peer=$([ $s = 0 ] && echo 02:00:00:00:00:b1 || echo 02:00:00:00:00:a1)
  side $s ip link set v$s up
  p=v$s
  if [ $layout = clear ]; then side $s ip link add link v$s name v$s.100 type vlan id 100; side $s ip link set v$s.100 up; p=v$s.100; fi
  side $s ip link add link $p name ms$s type macsec port 1 encrypt $enc send_sci $sci cipher $cipher
  tx=""; rx=""; pn="pn 1"
  case $cipher in *xpn*) pn="xpn ${XPN:-1}"; tx="salt 0102030405060708090a0b0c ssci $((s+1))"; rx="salt 0102030405060708090a0b0c ssci $((2-s))";; esac
  side $s ip macsec add ms$s tx sa 0 $pn on key 00 $key $tx
  side $s ip macsec add ms$s rx port 1 address $peer
  side $s ip macsec add ms$s rx port 1 address $peer sa 0 $pn on key 00 $key $rx
  side $s ip link set ms$s up
  a=ms$s
  if [ $layout = enc ]; then side $s ip link add link ms$s name ms$s.100 type vlan id 100; side $s ip link set ms$s.100 up; a=ms$s.100; fi
  side $s ip addr add 10.0.0.$((s+1))/24 dev $a
done
tcpdump -U -s0 -i v0 -w /out/$name.pcap 2>/dev/null & t=$!
sleep 1
ping -c 2 -i 0.2 -s 100 -W 1 10.0.0.2 >/dev/null || echo "$name: ping failed"
ip netns exec n1 ping -c 1 -s 1400 -W 1 10.0.0.1 >/dev/null || echo "$name: big ping failed"
sleep 0.5; kill $t; wait $t 2>/dev/null || true
ip -s macsec show ms0 | grep -A1 InPktsOK | tail -1 | awk -v n=$name '{print n": ms0 InPktsOK="$5" InPktsInvalid="$6" NotValid="$8}'
