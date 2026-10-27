FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY job-service/go.mod job-service/go.sum ./
RUN go mod download
COPY job-service/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /job-service .

FROM alpine:3.22
WORKDIR /app
COPY --from=builder /job-service .
USER 10001:10001
EXPOSE 50052
CMD ["./job-service"]
