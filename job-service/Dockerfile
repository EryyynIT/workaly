FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY job-service/go.mod job-service/go.sum ./
RUN go mod download
# Probe is independent of service sources: cached until the pin changes,
# and kept out of the builder's last layer. GOSUMDB=off matches the local
# go practice; the version pin is the verification for this utility tool.
RUN CGO_ENABLED=0 GOSUMDB=off go install github.com/grpc-ecosystem/grpc-health-probe@v0.4.55
COPY job-service/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /job-service .

FROM alpine:3.22
WORKDIR /app
COPY --from=builder /job-service .
COPY --from=builder /go/bin/grpc-health-probe /usr/local/bin/grpc_health_probe
USER 10001:10001
EXPOSE 50052
CMD ["./job-service"]
