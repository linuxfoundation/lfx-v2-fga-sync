# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT

# hadolint global ignore=DL3007

FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS builder

# Set necessary environment variables needed for our image. Allow building to
# other architectures via cross-compilation build-arg.
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH

# Move to working directory /build
WORKDIR /build

# Download dependencies to go modules cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the code into the container
COPY . .

# Build the packages
RUN go build -o /go/bin/fga-sync -trimpath -ldflags="-w -s" .

# Run our go binary standalone
FROM cgr.dev/chainguard/static:latest

# Implicit with base image; setting explicitly for linters. 65532 is the
# numeric UID of the Chainguard nonroot user.
USER 65532

COPY --from=builder /go/bin/fga-sync /fga-sync

ENTRYPOINT ["/fga-sync"]
