# syntax=docker/dockerfile:1
#
# The hub, built from source, with the laptop binaries it serves to installers and to laptops updating themselves.
# Run from the repository root:
#
#   docker build -f deploy/docker/Dockerfile -t murmur-hub .
#
# To sign the laptop binaries with your own release key (docs/development.md explains the key), pass the private
# key as a secret and the public key as a build argument. Pass both or neither: a hub built with the public key
# refuses to start with unsigned binaries.
#
#   MURMUR_RELEASE_KEY=<private key> docker build --secret id=release_key,env=MURMUR_RELEASE_KEY \
#     --build-arg RELEASE_PUBKEY=<public key> -f deploy/docker/Dockerfile -t murmur-hub .
#
# Releases don't use this file: GoReleaser builds the published image from its own binaries with Dockerfile.release,
# and its -litestream variant with Dockerfile.litestream. Here, --target litestream builds that variant.

ARG GO_VERSION=1.27.2

# The build stage runs on the machine's own platform and cross-compiles, which is much faster than emulating
# another CPU when building a multi-platform image.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
# the public key laptops check updates against; empty builds check checksums only
ARG RELEASE_PUBKEY=
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build <<EOF
set -eu
ldflags="-s -w -X github.com/EledoneAi/murmur/v2/internal/release.Version=${VERSION} -X github.com/EledoneAi/murmur/v2/internal/release.Key=${RELEASE_PUBKEY}"
mkdir -p /out/dist
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  os=${t%/*} arch=${t#*/}
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -tags laptop -o /out/dist/murmur-$os-$arch ./cmd/murmur
done
(cd /out/dist && sha256sum murmur-* > checksums.txt)
GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "$ldflags" -o /out/murmur-hub ./cmd/murmur-hub
EOF
# Without the secret, checksums.txt stays unsigned, which works only without RELEASE_PUBKEY. Laptops check the
# signature against the key built into them, so sign with a key they know if this hub is to update them.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=secret,id=release_key,required=false <<EOF
set -eu
if [ -s /run/secrets/release_key ]; then
  MURMUR_RELEASE_KEY="$(cat /run/secrets/release_key)" go run ./deploy/sign /out/dist/checksums.txt
fi
EOF

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS hub
LABEL org.opencontainers.image.title="murmur-hub" \
      org.opencontainers.image.description="The hub that links a team's murmur laptops" \
      org.opencontainers.image.source="https://github.com/EledoneAi/murmur" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/murmur-hub /usr/local/bin/murmur-hub
COPY --from=build /out/dist/ /usr/share/murmur/dist/
# WORKDIR creates /data owned by the image's user (nonroot), so a named volume mounted there starts out writable.
WORKDIR /data
VOLUME /data
ENV PORT=8080 \
    HOST=0.0.0.0 \
    STORE=sqlite:///data/murmur.db \
    DIST_DIR=/usr/share/murmur/dist
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/murmur-hub"]

# The same hub with Litestream, which copies the sqlite file to object storage as it changes and restores it when a
# new container starts without one (deploy/litestream). Build it with --target litestream. Litestream's image is a
# static binary; keep the version in step with Dockerfile.litestream.
FROM hub AS litestream
COPY --from=docker.io/litestream/litestream:0.5.17@sha256:4b02b9859a6b6b4087d8b8944e15f7e984bd7957cba322bbeee38b0e27b9656a \
     /usr/local/bin/litestream /usr/local/bin/litestream
COPY deploy/litestream/litestream.yml /etc/litestream.yml
ENTRYPOINT ["/usr/local/bin/litestream", "replicate", "-restore-if-db-not-exists", "-exec", "/usr/local/bin/murmur-hub"]

# Without --target, the build ends here: the plain hub.
FROM hub
