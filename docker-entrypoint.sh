#!/bin/sh
# Civ6Relay auto-tuning entrypoint
# Reads CIV6_TOTAL_MBPS (or auto-detects NIC speed), CIV6_MAX_MEMBERS,
# CIV6_MAX_ROOMS, CIV6_PORT, CIV6_SPEEDTEST_MBPS, CIV6_QUIET, CIV6_READERS
# and calculates per-member limits automatically.
#
# Auto-detection order for bandwidth:
#   1. CIV6_TOTAL_MBPS env var (explicit override)
#   2. /sys/class/net/<iface>/speed (kernel-reported NIC speed, Mbps)
#   3. fallback: 100 Mbps

set -e

# ── helpers ────────────────────────────────────────────────────────────────────

log() { printf '[entrypoint] %s\n' "$*" >&2; }

# Pick the first non-loopback, non-zero-speed NIC
detect_nic_mbps() {
    for iface in /sys/class/net/*; do
        name=$(basename "$iface")
        [ "$name" = "lo" ] && continue
        speed_file="$iface/speed"
        [ -r "$speed_file" ] || continue
        spd=$(cat "$speed_file" 2>/dev/null || echo 0)
        # speed=-1 means unknown (virtual NIC); skip
        [ "$spd" -gt 0 ] 2>/dev/null && echo "$spd" && return
    done
    echo 0
}

# ── bandwidth resolution ───────────────────────────────────────────────────────

if [ -n "${CIV6_TOTAL_MBPS:-}" ] && [ "${CIV6_TOTAL_MBPS}" -gt 0 ] 2>/dev/null; then
    TOTAL_MBPS="${CIV6_TOTAL_MBPS}"
    log "bandwidth: ${TOTAL_MBPS} Mbps (from CIV6_TOTAL_MBPS)"
else
    NIC_MBPS=$(detect_nic_mbps)
    if [ "${NIC_MBPS}" -gt 0 ] 2>/dev/null; then
        # Use 80% of reported NIC speed to leave room for overhead
        TOTAL_MBPS=$(( NIC_MBPS * 80 / 100 ))
        log "bandwidth: ${TOTAL_MBPS} Mbps (80%% of detected NIC ${NIC_MBPS} Mbps)"
    else
        TOTAL_MBPS=100
        log "bandwidth: ${TOTAL_MBPS} Mbps (default fallback)"
    fi
fi

# ── per-member limits ─────────────────────────────────────────────────────────
#
# A single Civ6 room has at most 8 players. With max-rooms concurrent,
# the worst case concurrent members is max_rooms * 8.  We allocate the
# total bandwidth evenly across that worst case, so every member always
# gets at least 1/N of capacity.
#
# Users can tune CIV6_MAX_MEMBERS to reflect their expected concurrency.
# Default: 100 concurrent members (plenty for a hobby server).

MAX_MEMBERS="${CIV6_MAX_MEMBERS:-100}"

# byte-rate = (total_mbps * 1_000_000 / 8) / max_members
TOTAL_BYTES_PER_SEC=$(( TOTAL_MBPS * 1000000 / 8 ))
MEMBER_BYTE_RATE=$(( TOTAL_BYTES_PER_SEC / MAX_MEMBERS ))
# Clamp to at least 1 MBps (8 Mbps per member minimum) to stay playable
[ "${MEMBER_BYTE_RATE}" -lt 1048576 ] && MEMBER_BYTE_RATE=1048576

# pps: 30 Mbps ≈ 5000 pps (server default for 1500-byte frames).
# Scale proportionally to byte-rate; floor at 200 pps.
DEFAULT_PPS_REF_BYTE_RATE=3750000   # 30 Mbps reference
DEFAULT_PPS=5000
MEMBER_PPS=$(( DEFAULT_PPS * MEMBER_BYTE_RATE / DEFAULT_PPS_REF_BYTE_RATE ))
[ "${MEMBER_PPS}" -lt 200 ] && MEMBER_PPS=200

log "per-member limits: byte_rate=${MEMBER_BYTE_RATE} B/s (~$(( MEMBER_BYTE_RATE * 8 / 1000000 )) Mbps), pps=${MEMBER_PPS}"

# ── other params ──────────────────────────────────────────────────────────────

PORT="${CIV6_PORT:-42056}"
MAX_ROOMS="${CIV6_MAX_ROOMS:-1024}"
SPEEDTEST_MBPS="${CIV6_SPEEDTEST_MBPS:-0}"
READERS="${CIV6_READERS:-0}"   # 0 = omit flag, server default is 1
QUIET_FLAG=""
[ "${CIV6_QUIET:-}" = "1" ] || [ "${CIV6_QUIET:-}" = "true" ] && QUIET_FLAG="-quiet"

# readers=0 means omit the flag entirely (server default = 1)
READERS_FLAG=""
[ "${READERS}" -gt 0 ] 2>/dev/null && READERS_FLAG="-readers ${READERS}"

log "starting: listen=:${PORT} max_rooms=${MAX_ROOMS} speedtest=${SPEEDTEST_MBPS} Mbps"

exec /civ6relay-server \
    -listen ":${PORT}" \
    -max-rooms "${MAX_ROOMS}" \
    -member-pps "${MEMBER_PPS}" \
    -member-byte-rate "${MEMBER_BYTE_RATE}" \
    -speedtest-mbps "${SPEEDTEST_MBPS}" \
    ${READERS_FLAG} \
    ${QUIET_FLAG} \
    "$@"
