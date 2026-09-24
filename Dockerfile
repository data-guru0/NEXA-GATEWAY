FROM golang:1.24-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/nexa ./cmd/nexa \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/nexa /nexa
COPY --chown=65532:65532 --from=build /out/data /data
VOLUME ["/data"]
EXPOSE 8080
ENV NEXA_ADDR=:8080 NEXA_DATA=/data
HEALTHCHECK --interval=15s --timeout=4s --start-period=5s --retries=3 CMD ["/nexa", "-healthcheck"]
ENTRYPOINT ["/nexa"]
