# Builds libepochd_preload.so against glibc 2.35 (Ubuntu 22.04), the version
# Redpanda bundles. A library built against an older glibc loads on newer ones,
# not the reverse, so this is the compatibility floor. Run build.sh instead of
# using this directly; it exports the .so to pkg/preload/.
FROM ubuntu:22.04 AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev binutils \
 && rm -rf /var/lib/apt/lists/*
COPY epochd_preload.c /src/epochd_preload.c
RUN mkdir /out \
 && gcc -std=gnu11 -shared -fPIC -O2 -Wall -Wextra -Werror \
        -Wl,--build-id=none -Wl,-z,relro,-z,now -Wl,--no-undefined -s \
        -o /out/libepochd_preload.so /src/epochd_preload.c -ldl \
 && echo "--- GLIBC symbol versions required ---" \
 && (objdump -T /out/libepochd_preload.so | grep -o 'GLIBC_[0-9.]*' | sort -uV)

FROM scratch AS artifact
COPY --from=build /out/libepochd_preload.so /libepochd_preload.so
