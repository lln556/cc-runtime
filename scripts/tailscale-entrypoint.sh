#!/bin/sh
set -eu
# SOCKS5 listener that the gateways use to reach the tailnet (TS_SOCKS5_SERVER).
socks_port="${SOCKS5_PORT:-1055}"
# Only the gateways may use it. Dev machines share this private network, so
# without this rule they could use the listener to bypass their gateway.
socks_clients="${SOCKS5_CLIENTS:-172.30.0.2 172.30.0.4}"

iptables -N CC_SOCKS 2>/dev/null || true
iptables -F CC_SOCKS
for ip in $socks_clients; do
    iptables -A CC_SOCKS -s "$ip/32" -j ACCEPT
done
iptables -A CC_SOCKS -p tcp -j REJECT --reject-with tcp-reset
iptables -C INPUT -p tcp --dport "$socks_port" -j CC_SOCKS 2>/dev/null ||
    iptables -I INPUT 1 -p tcp --dport "$socks_port" -j CC_SOCKS

exec /usr/local/bin/containerboot
