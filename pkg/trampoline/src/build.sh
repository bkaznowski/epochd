#!/usr/bin/env bash
# Rebuilds the vDSO hook blob from trampoline.c and link.ld, and regenerates
# trampoline.bin, trampoline.elf, offsets_generated.go and trampoline.src.sha256
# in pkg/trampoline (the parent of this directory). Needs only Docker.
#
# Commit the results: they are what Go embeds, so `go build` needs no C
# toolchain. The sources live in this subdirectory because Go rejects .c files
# in a package directory that is not using cgo.
set -euo pipefail
cd "$(dirname "$0")"
docker build --target artifact --output type=local,dest=.. .
echo "rebuilt pkg/trampoline: $(wc -c < ../trampoline.bin) bytes"
