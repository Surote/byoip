# byoip -- multi-stage, fully parameterized for disconnected/mirrored registries.
# Override BUILD_IMAGE / RUNTIME_IMAGE with --build-arg to point at an internal
# mirror; both stages otherwise touch the network only to pull the base image.

ARG BUILD_IMAGE=registry.access.redhat.com/ubi9/go-toolset:latest
ARG RUNTIME_IMAGE=registry.access.redhat.com/ubi9/ubi-micro:latest

FROM ${BUILD_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY style.css ./

# stdlib only -- no "go mod download" needed. GOPROXY=off makes the build
# fail loudly (instead of silently reaching the internet) if a non-stdlib
# dependency ever sneaks into go.mod.
ENV GOFLAGS=-mod=mod GOPROXY=off CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o /tmp/byoip .

FROM ${RUNTIME_IMAGE}
COPY --from=build /tmp/byoip /usr/local/bin/byoip
EXPOSE 8080

# Arbitrary-UID safe: single static binary, no writable state, nothing to
# chown. No USER numeric ID is fixed here -- OpenShift assigns a random UID
# in the root group at runtime (restricted-v2 SCC) and this image needs no
# special permissions to run under it.
USER 1001

ENTRYPOINT ["/usr/local/bin/byoip"]
