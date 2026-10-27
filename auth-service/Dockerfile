FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY auth-service/go.mod auth-service/go.sum ./
RUN go mod download
COPY auth-service/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /auth-service .

FROM alpine:3.22
WORKDIR /app
COPY --from=builder /auth-service .
USER 10001:10001
EXPOSE 8080
CMD ["./auth-service"]
