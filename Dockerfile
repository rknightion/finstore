# syntax=docker/dockerfile:1

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/finstore ./cmd/finstore

# The static runtime supplies the CA bundle required for HTTPS to the bank API and OTLP, and has
# no shell or package manager. UID/GID 65532 is nonroot.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/finstore /usr/local/bin/finstore
USER 65532:65532
EXPOSE 8080
# The check covers the process and its database only. A stalled sync is an alert, not a restart.
HEALTHCHECK --interval=30s --timeout=6s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/finstore", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/finstore"]
CMD ["serve"]
