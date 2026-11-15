#!/bin/sh
# Loads the preload library into this system's libc and checks that it works:
# `date` run with it preloaded must read a clock one year ahead, and the library
# must have counted itself in the state file. Run by the verify-* stages of the
# Dockerfile, once on glibc and once on musl.
set -eu
lib=${1:?usage: verify_load.sh LIB}
state=/tmp/epochd-state
year=31536000

# State file, little endian: offset_sec = one year, offset_nsec = 0,
# mask = 1 (enabled, advancing), seq = 0, attached = 0.
# 31536000 = 0x01E13380 -> bytes 80 33 E1 01.
printf '\200\063\341\001\000\000\000\000' >"$state"
printf '\000\000\000\000\000\000\000\000' >>"$state"
printf '\001\000\000\000\000\000\000\000' >>"$state"
printf '\000\000\000\000\000\000\000\000' >>"$state"
[ "$(wc -c <"$state")" -eq 32 ] || { echo "state file is not 32 bytes" >&2; exit 1; }

real=$(date -u +%s)
fake=$(LD_PRELOAD="$lib" EPOCHD_STATE_FILE="$state" date -u +%s)
diff=$((fake - real))
if [ "$diff" -lt $((year - 5)) ] || [ "$diff" -gt $((year + 5)) ]; then
    echo "FAIL: clock moved by ${diff}s, want ${year}s (+-5)" >&2
    exit 1
fi

attached=$(od -An -tu4 -j28 -N4 "$state" | tr -d ' ')
if [ "$attached" -lt 1 ]; then
    echo "FAIL: the library did not count itself in the state file (attached=$attached)" >&2
    exit 1
fi
echo "ok: $(. /etc/os-release && echo "$PRETTY_NAME"): clock +${diff}s, attached=$attached"
