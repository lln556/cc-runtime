#!/bin/sh
set -eu
: "${PASSWORD:?Set PASSWORD in .env}"

config=/root/.config/mihomo/config.yaml
if grep -Eq 'REPLACE_|YOUR_' "$config"; then
    echo 'Fill in proxy/config.yaml before starting the gateway.' >&2
    exit 1
fi
/mihomo -t -f "$config"

# TCP is terminated by Mihomo locally, not forwarded by the kernel.
# This also blocks UDP/ICMP and prevents direct fallback when Mihomo stops.
iptables -P FORWARD DROP
iptables -N CC_INPUT 2>/dev/null || true
iptables -F CC_INPUT
iptables -A CC_INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A CC_INPUT -p tcp -m multiport --dports 53,7893 -j ACCEPT
iptables -A CC_INPUT -p udp --dport 53 -j ACCEPT
iptables -A CC_INPUT -j REJECT
iptables -C INPUT -s 172.30.0.10/32 -j CC_INPUT 2>/dev/null ||
    iptables -I INPUT 1 -s 172.30.0.10/32 -j CC_INPUT

iptables -t nat -N CC_REDIRECT 2>/dev/null || true
iptables -t nat -F CC_REDIRECT
# Capture explicit DNS destinations too, before general TCP redirection.
iptables -t nat -A CC_REDIRECT -p udp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -A CC_REDIRECT -p tcp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -A CC_REDIRECT -p tcp -j REDIRECT --to-ports 7893
iptables -t nat -C PREROUTING -s 172.30.0.10/32 -j CC_REDIRECT 2>/dev/null ||
    iptables -t nat -I PREROUTING 1 -s 172.30.0.10/32 -j CC_REDIRECT

exec /mihomo -f "$config" -ext-ctl 0.0.0.0:9090 -secret "$PASSWORD"
