FROM rust:1.96.0-slim-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler pkg-config ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY contracts/ contracts/
COPY user-service/ user-service/
RUN cargo build --locked --release --manifest-path user-service/Cargo.toml

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/user-service/target/release/user-service /usr/local/bin/user-service
USER 10001:10001
EXPOSE 50051
CMD ["user-service"]
