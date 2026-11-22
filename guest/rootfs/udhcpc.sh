#!/bin/busybox sh
# What udhcpc runs when it has a lease: the address, the route, the resolver.
case "$1" in
bound | renew)
	ip addr flush dev "$interface"
	ip addr add "$ip/${mask:-24}" dev "$interface"
	[ -n "$router" ] && ip route replace default via "${router%% *}" dev "$interface"
	: > /etc/resolv.conf
	for d in $dns; do echo "nameserver $d" >> /etc/resolv.conf; done
	;;
esac
