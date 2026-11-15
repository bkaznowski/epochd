#!/usr/bin/env bash
# Does LD_PRELOAD interception work for Redpanda?
#
#   ./run.sh [offset-seconds]     (default 864000 = 10 days)
#
# 1. Build probe.so against glibc 2.35 and bake it into a Redpanda image.
# 2. Control: run `date` (system loader, system glibc) under the probe. If this
#    fails the probe itself is broken, and step 3 means nothing.
# 3. Run Redpanda (bundled ld.so + glibc 2.35) under the probe for ~25s, then
#    report which libc time functions it called and whether its own log
#    timestamps moved by the offset.
set -u
cd "$(dirname "$0")"
export MSYS_NO_PATHCONV=1   # Git Bash on Windows: don't rewrite /opt/... paths

OFFSET="${1:-864000}"
IMAGE=epochd-preload-probe
LIB=/opt/epochd/probe.so
OUT="$(mktemp -d)"
want_date="$(date -u -d "@$(( $(date +%s) + OFFSET ))" +%F)"
real_date="$(date -u +%F)"

echo "== build =="
docker build -t "$IMAGE" . || { echo "BUILD FAILED"; exit 2; }

echo
echo "== control: date under the probe (expect $want_date, real is $real_date) =="
got="$(docker run --rm --entrypoint date -e LD_PRELOAD=$LIB -e EPOCHD_PROBE_OFFSET_SEC="$OFFSET" "$IMAGE" -u +%F 2>"$OUT/control.err")"
cat "$OUT/control.err"
if [ "$got" = "$want_date" ]; then
  echo "control PASS ($got)"
else
  echo "control FAIL: got '$got', want '$want_date' -- probe is not working; aborting"
  exit 2
fi

echo
echo "== redpanda under the probe (offset ${OFFSET}s) =="
cid="$(docker run -d \
  -e LD_PRELOAD=$LIB -e EPOCHD_PROBE_OFFSET_SEC="$OFFSET" \
  "$IMAGE" redpanda start --overprovisioned --smp 1 --memory 1G --reserve-memory 0M \
  --node-id 0 --check=false)" || { echo "could not start redpanda"; exit 2; }
sleep 25
docker stop -t 15 "$cid" >/dev/null
docker logs "$cid" >"$OUT/redpanda.log" 2>&1
docker rm "$cid" >/dev/null

echo "-- probe lines from the redpanda process --"
grep '\[epochd-probe\]' "$OUT/redpanda.log" | grep 'comm=redpanda' | sort -u
echo
echo "-- probe lines from other processes (wrapper, rpk, ...) --"
grep '\[epochd-probe\]' "$OUT/redpanda.log" | grep -v 'comm=redpanda' | sort -u | head -20
echo
echo "-- redpanda log timestamps (first 5) --"
grep -E '^(INFO|WARN|ERROR|DEBUG|TRACE) +[0-9]{4}-' "$OUT/redpanda.log" | head -5
echo

fired=$(grep '\[epochd-probe\]' "$OUT/redpanda.log" | grep -c 'comm=redpanda first ')
shifted_logs=$(grep -E '^(INFO|WARN|ERROR) +[0-9]{4}-' "$OUT/redpanda.log" | grep -c " $want_date")
real_logs=$(grep -E '^(INFO|WARN|ERROR) +[0-9]{4}-' "$OUT/redpanda.log" | grep -c " $real_date")

echo "== verdict =="
echo "probe first-call lines in redpanda: $fired | log lines on shifted date: $shifted_logs | on real date: $real_logs"
if [ "$fired" -gt 0 ] && [ "$shifted_logs" -gt 0 ]; then
  echo "INTERCEPTED: redpanda's wall clock goes through libc; LD_PRELOAD works."
elif [ "$fired" -gt 0 ]; then
  echo "PARTIAL: libc time calls were seen but redpanda's log timestamps did not move;"
  echo "         its wall clock must come from somewhere else (vDSO direct, raw syscall)."
else
  echo "NOT INTERCEPTED: no libc time calls from redpanda; use the vDSO path."
  echo "         (if no 'loaded' line appears at all, the preload did not load; check the log below)"
fi
echo
echo "full redpanda output: $OUT/redpanda.log"
