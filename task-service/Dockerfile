FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY task-service/go.mod task-service/go.sum ./
RUN go mod download
COPY task-service/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /task-service .

FROM alpine:3.22
WORKDIR /app
COPY --from=builder /task-service .
USER 10001:10001
EXPOSE 50053
CMD ["./task-service"]
