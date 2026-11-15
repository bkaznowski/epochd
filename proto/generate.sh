#!/usr/bin/env bash
# Regenerates pkg/agentpb from proto/agent/v1/agent.proto. Needs only Docker and
# network access to fetch the pinned generator plugins. Commit the result.
set -euo pipefail
cd "$(dirname "$0")"
docker build --target artifact --output type=local,dest=.. .
echo "regenerated $(cd .. && pwd)/pkg/agentpb"
