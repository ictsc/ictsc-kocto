FROM --platform=$BUILDPLATFORM golang:1.26@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/bundle/ internal/bundle/
COPY cmd/bundle-builder/ cmd/bundle-builder/
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /bundle-builder ./cmd/bundle-builder

FROM debian:trixie-slim
# Python yq (jq syntax) matches the upstream builder, not mikefarah/yq.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git bash jq yq skopeo e2fsprogs qemu-utils \
    && rm -rf /var/lib/apt/lists/*
# Pin the guest/bundle contract. Never fetch scripts from Problem repositories.
ARG CLAB_IMAGE_COMMIT=001a91ea318d239e5f83d2764ebee0e9c90708ab
RUN git clone https://github.com/ictsc/kocto-clab-image.git /upstream \
    && cd /upstream && git checkout --detach "$CLAB_IMAGE_COMMIT" \
    && test "$(git rev-parse HEAD)" = "$CLAB_IMAGE_COMMIT" \
    && mkdir -p /opt/bundle && cp build/build-bundle.sh /opt/bundle/ \
    && rm -rf /upstream
ARG VCS_REF=local
LABEL org.opencontainers.image.source="https://github.com/ictsc/ictsc-kocto" \
      org.opencontainers.image.revision="$VCS_REF" \
      lab.icttoracon.net.clab-image-commit="$CLAB_IMAGE_COMMIT"
COPY --from=builder /bundle-builder /usr/local/bin/bundle-builder
# KubeVirt requires raw disk.img owned by QEMU UID 107.
USER 107:107
ENV HOME=/tmp TMPDIR=/tmp
ENTRYPOINT ["/usr/local/bin/bundle-builder"]
