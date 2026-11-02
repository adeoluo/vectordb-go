# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/adeoluo/vectordb-go/internal/cli.Version=${VERSION}" \
    -o /out/vectordb ./cmd/vectordb \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vectordb /usr/local/bin/vectordb
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/vectordb"]
CMD ["serve", "--data-dir", "/data", "--addr", "0.0.0.0:8080"]
