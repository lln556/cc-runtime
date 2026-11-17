#!/bin/sh
set -eu
: "${PASSWORD:?Set PASSWORD in .env}"
# Gateway private address and the single client it serves (defaults: main gateway/dev).
gateway_ip="${GATEWAY_IP:-172.30.0.2}"
client_ip="${CLIENT_IP:-172.30.0.10}"

config=/root/.config/mihomo/config.yaml
if grep -Eq 'REPLACE_|airport\.example\.com|residential\.example\.com' "$config"; then
    echo 'Fill in proxy/config-pennsylvania.yaml with your airport node and residential proxy first.' >&2
    exit 1
fi
/mihomo -t -f "$config"

# TCP is terminated by Mihomo locally, not forwarded by the kernel.
# This also blocks UDP/ICMP and prevents direct fallback when Mihomo stops.
iptables -P FORWARD DROP
iptables -N CC_INPUT 2>/dev/null || true
iptables -F CC_INPUT
iptables -A CC_INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
# Allow only the private host tunnels (SSH, remote-shell) before rejecting other dev traffic.
iptables -A CC_INPUT -d "$gateway_ip/32" -p tcp -m multiport --dports 10023,10024 -j ACCEPT
iptables -A CC_INPUT -p tcp -m multiport --dports 53,7893 -j ACCEPT
iptables -A CC_INPUT -p udp --dport 53 -j ACCEPT
iptables -A CC_INPUT -j REJECT
# Optional host-only mixed port: never reachable from the private network.
iptables -C INPUT -s 172.30.0.0/24 -p tcp --dport 7890 -j REJECT 2>/dev/null ||
    iptables -I INPUT 1 -s 172.30.0.0/24 -p tcp --dport 7890 -j REJECT
iptables -C INPUT -s "$client_ip/32" -j CC_INPUT 2>/dev/null ||
    iptables -I INPUT 1 -s "$client_ip/32" -j CC_INPUT

iptables -t nat -N CC_REDIRECT 2>/dev/null || true
iptables -t nat -F CC_REDIRECT
# Keep the fixed host tunnels (SSH, remote-shell) out of transparent TCP interception.
iptables -t nat -A CC_REDIRECT -d "$gateway_ip/32" -p tcp -m multiport --dports 10023,10024 -j RETURN
# Capture explicit DNS destinations too, before general TCP redirection.
iptables -t nat -A CC_REDIRECT -p udp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -A CC_REDIRECT -p tcp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -A CC_REDIRECT -p tcp -j REDIRECT --to-ports 7893
iptables -t nat -C PREROUTING -s "$client_ip/32" -j CC_REDIRECT 2>/dev/null ||
    iptables -t nat -I PREROUTING 1 -s "$client_ip/32" -j CC_REDIRECT

exec /mihomo -f "$config" -ext-ctl 0.0.0.0:9090 -secret "$PASSWORD"
