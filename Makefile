CLUSTER          := epochd-dev
IMAGE_TAG        := dev
AGENT_IMAGE      := epochd-agent:$(IMAGE_TAG)
CONTROLLER_IMAGE := epochd-controller:$(IMAGE_TAG)

.PHONY: check-deps cluster delete-cluster images load deploy e2e

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
