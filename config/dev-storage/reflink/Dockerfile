# The upstream driver supplies the CSI lifecycle; this patch only changes
# Filesystem snapshots/clones to require shared XFS extents in the dev cluster.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETARCH
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends patch \
    && rm -rf /var/lib/apt/lists/*
ADD https://codeload.github.com/kubernetes-csi/csi-driver-host-path/tar.gz/refs/tags/v1.18.0 /tmp/hostpath.tar.gz
RUN echo '03d581877ed8a0a87851c59ca3d50b04663e17f8d5310eb964002b0a7fd73421  /tmp/hostpath.tar.gz' | sha256sum -c - \
    && tar -xzf /tmp/hostpath.tar.gz --strip-components=1 -C /src
COPY reflink.patch /tmp/reflink.patch
RUN patch -p1 < /tmp/reflink.patch
# These sources belong to the upstream hostpath package. The ignore tag keeps
# controller-gen and the manager's Go tests from loading them in this repository.
COPY reflink.go reflink_test.go /src/pkg/hostpath/
RUN test -z "$(gofmt -l pkg/hostpath/reflink.go pkg/hostpath/reflink_test.go)" \
    && sed -i '/^\/\/go:build ignore$/d' pkg/hostpath/reflink.go pkg/hostpath/reflink_test.go \
    && gofmt -w pkg/hostpath/reflink.go pkg/hostpath/reflink_test.go
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go test ./pkg/hostpath ./pkg/state \
    && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags='-X main.version=v1.18.0-kocto-reflink' -o /hostpathplugin ./cmd/hostpathplugin \
    && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go test -c -o /reflink-tests ./pkg/hostpath

FROM registry.k8s.io/sig-storage/hostpathplugin:v1.18.0
# xfs_io lets the verification inspect actual shared physical extents.
RUN apk add --no-cache xfsprogs-extra && xfs_io -V
COPY --from=builder /hostpathplugin /hostpathplugin
COPY --from=builder /reflink-tests /reflink-tests
RUN /hostpathplugin --version | grep -F 'v1.18.0-kocto-reflink'
ENTRYPOINT ["/hostpathplugin"]
