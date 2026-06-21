CLUSTER          := epochd-dev
TEST_CLUSTER     := epochd-test
IMAGE_TAG        := dev
AGENT_IMAGE      := epochd-agent:$(IMAGE_TAG)
CONTROLLER_IMAGE := epochd-controller:$(IMAGE_TAG)

.PHONY: check-deps cluster delete-cluster images load deploy e2e \
        test-integration _integration-inner kind-up-test kind-down-test

check-deps:
	@command -v kind    >/dev/null || (echo "ERROR: kind not found.    Install: https://kind.sigs.k8s.io"; exit 1)
	@command -v kubectl >/dev/null || (echo "ERROR: kubectl not found. Install: https://kubernetes.io/docs/tasks/tools/"; exit 1)
	@command -v docker  >/dev/null || (echo "ERROR: docker not found.  Install: https://docs.docker.com/get-docker/"; exit 1)

# Create a local kind cluster. Safe to run multiple times — kind will error if
# the cluster already exists, which is fine.
cluster: check-deps
	kind create cluster --name $(CLUSTER)

delete-cluster:
	kind delete cluster --name $(CLUSTER)

# Build both images for linux/amd64.
images:
	docker build -f Dockerfile.agent      --platform linux/amd64 -t $(AGENT_IMAGE) .
	docker build -f Dockerfile.controller --platform linux/amd64 -t $(CONTROLLER_IMAGE) .

# Load the locally built images into the kind cluster so pods can use them
# without a registry push.
load: images
	kind load docker-image $(AGENT_IMAGE)      --name $(CLUSTER)
	kind load docker-image $(CONTROLLER_IMAGE) --name $(CLUSTER)

# Apply manifests and patch the images to the locally built tags.
# `kubectl set image` is used so the production manifests (which reference
# ghcr.io) stay unchanged.
deploy: load
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/daemonset.yaml
	kubectl set image daemonset/epochd-agent agent=$(AGENT_IMAGE) -n epochd
	kubectl rollout status daemonset/epochd-agent -n epochd --timeout=60s
	kubectl apply -f deploy/controller-deployment.yaml
	kubectl set image deployment/epochd-controller controller=$(CONTROLLER_IMAGE) -n epochd
	kubectl rollout status deployment/epochd-controller -n epochd --timeout=60s

# Run the e2e suite. Opens a port-forward to the controller, runs the tests,
# then kills the port-forward regardless of test outcome.
e2e: deploy
	@echo "Starting port-forward to epochd-controller..."
	kubectl port-forward svc/epochd-controller 18080:80 -n epochd & \
	PF_PID=$$!; \
	sleep 2; \
	EPOCHD_URL=http://localhost:18080 go test ./e2e/... -v -tags=e2e -timeout=5m; \
	TEST_EXIT=$$?; \
	kill $$PF_PID 2>/dev/null || true; \
	exit $$TEST_EXIT

# ---------------------------------------------------------------------------
# Self-contained integration test harness (Phase 33)
#
# Spins up a dedicated kind cluster, builds and loads images, deploys epochd,
# runs the e2e suite, and tears the cluster down — even on failure.
#
# Usage:
#   make test-integration
# ---------------------------------------------------------------------------

# Create the integration-test cluster using the checked-in kind config.
kind-up-test: check-deps
	kind create cluster --name $(TEST_CLUSTER) --config deploy/kind-config.yaml

# Tear down the integration-test cluster.
kind-down-test:
	kind delete cluster --name $(TEST_CLUSTER)

# Build, load, deploy, and test inside the ephemeral cluster.
# Separated so that test-integration can always run kind-down-test after it.
_integration-inner: images
	kind load docker-image $(AGENT_IMAGE)      --name $(TEST_CLUSTER)
	kind load docker-image $(CONTROLLER_IMAGE) --name $(TEST_CLUSTER)
	kubectl apply -f deploy/ --context kind-$(TEST_CLUSTER)
	kubectl --context kind-$(TEST_CLUSTER) -n epochd rollout status \
	    deployment/epochd-controller --timeout=60s
	kubectl --context kind-$(TEST_CLUSTER) -n epochd rollout status \
	    daemonset/epochd-agent       --timeout=60s
	@echo "Starting port-forward to epochd-controller in kind-$(TEST_CLUSTER)..."
	kubectl --context kind-$(TEST_CLUSTER) \
	    port-forward svc/epochd-controller 18080:80 -n epochd & \
	PF_PID=$$!; \
	sleep 2; \
	EPOCHD_URL=http://localhost:18080 go test ./e2e/... -v -tags=e2e -timeout=5m; \
	TEST_EXIT=$$?; \
	kill $$PF_PID 2>/dev/null || true; \
	exit $$TEST_EXIT

# Fully self-contained: create cluster, run tests, delete cluster.
# The cluster is deleted even when tests fail.
test-integration: check-deps
	$(MAKE) kind-up-test
	$(MAKE) _integration-inner || ($(MAKE) kind-down-test; exit 1)
	$(MAKE) kind-down-test
