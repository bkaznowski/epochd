#!/bin/sh
# Compiles deadline_test.c and runs it under the preload library with the clock
# one day ahead, one day behind, and frozen; each primitive must really time out
# after about 300 ms. Run by the verify-* stages of the Dockerfile (glibc and
# musl), where a C compiler has been installed.
set -eu
lib=${1:?usage: verify_deadlines.sh LIB}
state=/tmp/epochd-deadline-state
cc -O1 -o /tmp/deadline_test /deadline_test.c -lpthread -lrt 2>/dev/null || cc -O1 -o /tmp/deadline_test /deadline_test.c -lpthread

# Layout: offset_sec(8) offset_nsec(8) mask(8) seq(4) attached(4), little endian.
write_state() { # $1 = 8 octal-escaped bytes of offset_sec, $2 = 8 octal-escaped bytes of mask
    printf "$1" >"$state"
    printf '\000\000\000\000\000\000\000\000' >>"$state"
    printf "$2" >>"$state"
    printf '\000\000\000\000\000\000\000\000' >>"$state"
}

run() {
    echo "== $1"
    # timeout: without translation these would block for a day
    LD_PRELOAD="$lib" EPOCHD_STATE_FILE="$state" timeout 120 /tmp/deadline_test
}

# +86400 s = 0x15180 -> 80 51 01
write_state '\200\121\001\000\000\000\000\000' '\001\000\000\000\000\000\000\000'
run "clock one day ahead"

# -86400 s = 0xFFFFFFFFFFFEAE80 -> 80 AE FE FF FF FF FF FF
write_state '\200\256\376\377\377\377\377\377' '\001\000\000\000\000\000\000\000'
run "clock one day behind"

# frozen at 2,000,000,000 = 0x77359400 -> 00 94 35 77 ; mask = enabled|frozen
write_state '\000\224\065\167\000\000\000\000' '\003\000\000\000\000\000\000\000'
run "clock frozen"
