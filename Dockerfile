# syntax=docker/dockerfile:1
# Base images are pinned by digest; Dependabot (.github/dependabot.yml) proposes updates.

FROM --platform=$BUILDPLATFORM node:26.13.1-bookworm-slim@sha256:5df79989ed685e09f0a98019db4cebfb22fb9df1a53c0161664a011f7ff6b0f1 AS portal-build
WORKDIR /src/web
COPY web/package*.json ./
COPY web/scripts/ scripts/
RUN npm ci
COPY web/ ./
RUN npm run build
RUN node scripts/licenses.mjs /out/portal-notices.txt

# The Go version is pinned here and in go.mod; change both together.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
COPY LICENSE ./
RUN go mod download
RUN GOBIN=/usr/local/bin go install github.com/google/go-licenses/v2@v2.0.1
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/assets.go web/assets.go
COPY --from=portal-build /src/web/dist web/dist
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go-licenses check ./cmd/olr --allowed_licenses=MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,0BSD,CC0-1.0,Unlicense \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go-licenses save ./cmd/olr --save_path=/out/licenses/go
COPY --from=portal-build /out/portal-notices.txt /out/licenses/portal-notices.txt
RUN cp LICENSE /out/licenses/LICENSE \
 && cp /usr/local/go/LICENSE /out/licenses/GO-LICENSE \
 && printf 'Third-party Go and portal dependency license texts\n' > /out/licenses/THIRD_PARTY_NOTICES.txt \
 && find /out/licenses/go -type f -exec sh -c 'for source do printf "\n--- %s ---\n" "$source"; cat "$source"; done' sh {} + >> /out/licenses/THIRD_PARTY_NOTICES.txt \
 && cat /out/licenses/GO-LICENSE /out/licenses/portal-notices.txt >> /out/licenses/THIRD_PARTY_NOTICES.txt
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/arm2arm/open-llm-router/internal/buildinfo.Version=$VERSION -X github.com/arm2arm/open-llm-router/internal/buildinfo.Commit=$COMMIT" -o /out/olr ./cmd/olr \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/mock-llm ./cmd/mock-llm \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/olr-loadtest ./cmd/olr-loadtest

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS loadtest
COPY --from=build /out/olr-loadtest /usr/local/bin/olr-loadtest
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/olr-loadtest"]

# Build with `docker build --target mock-llm -t olr-mock-llm .` when only
# the deterministic test backend is needed.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS mock-llm
COPY --from=build /out/mock-llm /usr/local/bin/mock-llm
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mock-llm"]

# Include Bash and curl for diagnostics from the running router container.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS olr
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates curl passwd \
 && groupadd --gid 65532 nonroot \
 && useradd --uid 65532 --gid nonroot --no-log-init --create-home --shell /bin/bash nonroot \
 && rm -rf /var/lib/apt/lists/*
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.source="https://github.com/arm2arm/open-llm-router" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/olr /usr/local/bin/olr
COPY --from=build /out/licenses /licenses
# The image carries no config. Mount the site's file at this path.
ENV OLR_CONFIG=/etc/olr/config.yaml
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/olr"]
CMD ["serve"]
