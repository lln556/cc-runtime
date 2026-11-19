#!/bin/sh
set -eu

GATEWAY="${GATEWAY_IP:-172.30.0.2}"

configure() {
    # This runs inside dev's network namespace, not proxy's.
    # Prevent Docker's embedded DNS from becoming an alternate resolver.
    iptables -C OUTPUT -d 127.0.0.11/32 -j REJECT 2>/dev/null ||
        iptables -I OUTPUT 1 -d 127.0.0.11/32 -j REJECT

    # Configure the default route after blocking the alternate DNS path.
    case " $(ip route list 0.0.0.0/0) " in
        *" via $GATEWAY "*) ;;
        *) ip route replace default via "$GATEWAY" ;;
    esac
}

configure
echo "dev default route ($GATEWAY) and DNS interception configured."

sleep_pid=""
trap '[ -z "$sleep_pid" ] || kill "$sleep_pid" 2>/dev/null || true; exit 0' TERM INT

# Stay alive so Docker restarts this helper across host reboots, and exit
# non-zero if the target container's network namespace was replaced (e.g.
# `docker restart dev-*`), letting Docker re-attach us to the new namespace.
while :; do
    sleep 5 &
    sleep_pid=$!
    wait "$sleep_pid" || true
    sleep_pid=""
    configure
done
