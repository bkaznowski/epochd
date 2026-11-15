# Regenerates pkg/agentpb from proto/agent/v1/agent.proto without the Buf
# registry: buf compiles the proto itself and runs the two generator plugins
# locally, pinned to the versions the committed files were produced with. Run
# generate.sh rather than using this directly.
FROM bufbuild/buf:latest AS buf

FROM golang:1.26 AS gen
COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf
RUN GOBIN=/usr/local/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5 \
 && GOBIN=/usr/local/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
WORKDIR /w
# The module root is this directory, so the proto's import path is agent/v1/agent.proto.
COPY agent agent
RUN printf 'version: v2\nmodules:\n  - path: .\n' > buf.yaml \
 && printf 'version: v2\nplugins:\n  - local: protoc-gen-go\n    out: /out\n    opt: [module=epochd]\n  - local: protoc-gen-go-grpc\n    out: /out\n    opt: [module=epochd]\n' > buf.gen.yaml \
 && buf generate

FROM scratch AS artifact
COPY --from=gen /out/ /
