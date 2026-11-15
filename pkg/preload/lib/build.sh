#!/usr/bin/env bash
# Rebuilds pkg/preload/libepochd_preload.so, the library embedded by pkg/preload.
# Run after editing epochd_preload.c and commit the resulting .so, the same way
# pkg/trampoline/trampoline.bin is committed, so `go build` needs no C toolchain.
#
# Before exporting it, the finished library is loaded into a real glibc system and
# a real musl (Alpine) system; the build fails if either does not see it work.
set -euo pipefail
cd "$(dirname "$0")"
docker build --target verify-glibc --progress=plain .
docker build --target verify-musl --progress=plain .
docker build --target artifact --output type=local,dest=.. .
echo "wrote $(cd .. && pwd)/libepochd_preload.so"
