FROM node:22-bookworm AS web-build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN --network=none npm run build && npm test

FROM golang:1.26-bookworm AS source
RUN apt-get update && apt-get install -y --no-install-recommends ripgrep python3 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /web/dist ./web/dist

FROM source AS build
RUN --network=none PWNMESH_WEB_PREBUILT=1 bash scripts/build.sh /out/pwnmesh

FROM build AS test
# Release checks use the same isolated entrypoint as CI.
RUN --network=none PWNMESH_WEB_PREBUILT=1 bash scripts/check.sh go

FROM debian:bookworm-slim AS runtime-base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash ripgrep \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/pwnmesh"]
CMD ["serve", "--host", "0.0.0.0", "--db-path", "/data/pwnmesh.db"]

# Explicit opt-in for development; the default final target retains validation.
FROM runtime-base AS development
COPY --from=build /out/pwnmesh /usr/local/bin/pwnmesh

FROM runtime-base AS runtime
COPY --from=test /out/pwnmesh /usr/local/bin/pwnmesh
