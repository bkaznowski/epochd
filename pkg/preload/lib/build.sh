#!/usr/bin/env bash
# Rebuilds pkg/preload/libepochd_preload.so, the library embedded by pkg/preload.
# Run after editing epochd_preload.c and commit the resulting .so, the same way
# pkg/trampoline/trampoline.bin is committed, so `go build` needs no C toolchain.
set -euo pipefail
cd "$(dirname "$0")"
docker build --target artifact --output type=local,dest=.. .
echo "wrote $(cd .. && pwd)/libepochd_preload.so"
