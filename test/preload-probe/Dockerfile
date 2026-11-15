# Build the probe against glibc 2.35 (Ubuntu 22.04) -- the version Redpanda
# bundles -- then drop it into the Redpanda image.
FROM ubuntu:22.04 AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*
COPY probe.c /src/probe.c
RUN gcc -shared -fPIC -O2 -Wall -Wextra -o /probe.so /src/probe.c -ldl \
 && echo "--- GLIBC versions the probe requires ---" \
 && (objdump -T /probe.so | grep -o 'GLIBC_[0-9.]*' | sort -uV)

FROM redpandadata/redpanda:latest
COPY --from=build /probe.so /opt/epochd/probe.so
