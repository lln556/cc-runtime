#!/bin/sh
set -eu

# dev joins this container's network namespace without receiving NET_ADMIN.
check_network() {
    ip route show default | grep -q '^default via 172.30.0.2 ' &&
        iptables -C OUTPUT -d 127.0.0.11/32 -j REJECT 2>/dev/null
}

if [ "${1:-}" = "--check" ]; then
    check_network
    exit $?
fi

configure_network() {
    # Block Docker DNS before enabling the gateway route.
    iptables -C OUTPUT -d 127.0.0.11/32 -j REJECT 2>/dev/null ||
        iptables -I OUTPUT 1 -d 127.0.0.11/32 -j REJECT
    ip route replace default via 172.30.0.2
    echo 'dev default route and DNS interception configured.'
}

configure_network
if [ "${1:-}" = "--once" ]; then
    exit 0
fi

trap 'exit 0' TERM INT
while :; do
    sleep 2 &
    wait $! || exit 0
    if ! check_network; then
        configure_network
    fi
done
