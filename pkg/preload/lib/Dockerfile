# Builds libepochd_preload.so against glibc 2.35 (Ubuntu 22.04), the version
# Redpanda bundles. A library built against an older glibc loads on newer ones,
# not the reverse, so this is the compatibility floor. Run build.sh instead of
# using this directly; it exports the .so to pkg/preload/.
#
# The one file also has to load under musl (Alpine). Two things in a default
# Ubuntu build would stop that, so they are switched off:
#
#   - Ubuntu's gcc enables _FORTIFY_SOURCE and the stack protector by default,
#     which turns dprintf into __dprintf_chk and so on; musl has no *_chk.
#   - Linking against libc records a DT_NEEDED on libc.so.6 (and versioned symbol
#     references), and musl's loader has no libc.so.6 to satisfy it. Linking with
#     -nodefaultlibs leaves the library with no dependencies at all: every symbol
#     it uses (dlsym, mmap, getenv, ...) is resolved from the process's own libc
#     when the library is loaded, which is how an LD_PRELOAD library normally
#     reaches libc anyway.
#
# The checks below fail the build if either regresses, and the verify-* stages
# load the finished library into a real glibc and a real musl system and check
# that it moves the clock.
FROM ubuntu:22.04 AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev binutils \
 && rm -rf /var/lib/apt/lists/*
COPY epochd_preload.c /src/epochd_preload.c
RUN mkdir /out \
 && gcc -std=gnu11 -shared -fPIC -O2 -Wall -Wextra -Werror \
        -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 -fno-stack-protector \
        -nodefaultlibs \
        -Wl,--build-id=none -Wl,-z,relro,-z,now -s \
        -o /out/libepochd_preload.so /src/epochd_preload.c \
 && echo "--- dynamic dependencies (must be none) ---" \
 && ! readelf -d /out/libepochd_preload.so | grep NEEDED \
 && echo "--- undefined symbols ---" \
 && nm -D --undefined-only /out/libepochd_preload.so \
 && echo "--- must not need glibc-only helpers ---" \
 && ! nm -D --undefined-only /out/libepochd_preload.so | grep -E '_chk|__stack_chk|GLIBC_' \
 && echo "ok"

FROM scratch AS artifact
COPY --from=build /out/libepochd_preload.so /libepochd_preload.so

# --- verification: load the library into real systems ------------------------
FROM ubuntu:22.04 AS verify-glibc
COPY --from=build /out/libepochd_preload.so /lib/libepochd_preload.so
COPY verify_load.sh /verify_load.sh
RUN sh /verify_load.sh /lib/libepochd_preload.so

FROM alpine:3.20 AS verify-musl
COPY --from=build /out/libepochd_preload.so /lib/libepochd_preload.so
COPY verify_load.sh /verify_load.sh
RUN sh /verify_load.sh /lib/libepochd_preload.so
