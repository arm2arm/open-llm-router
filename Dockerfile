# syntax=docker/dockerfile:1

# The Go version is pinned here and in go.mod; change both together.
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/olr ./cmd/olr \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-llm ./cmd/mock-llm

# Build with `docker build --target mock-llm -t olr-mock-llm .` when only
# the deterministic test backend is needed.
FROM gcr.io/distroless/static-debian13:nonroot AS mock-llm
COPY --from=build /out/mock-llm /usr/local/bin/mock-llm
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mock-llm"]

# Distroless static: no shell, no package manager; runs as user 65532.
FROM gcr.io/distroless/static-debian13:nonroot AS olr
COPY --from=build /out/olr /usr/local/bin/olr
# The image carries no config. Mount the site's file at this path.
ENV OLR_CONFIG=/etc/olr/config.yaml
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/olr"]
CMD ["serve"]
