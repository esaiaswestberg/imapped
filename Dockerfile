FROM --platform=$BUILDPLATFORM rust:1-bookworm AS builder
ARG TARGETPLATFORM
COPY --from=tonistiigi/xx:1.4.0 / /

# Install clang and lld for cross-compilation
RUN apt-get update && apt-get install -y clang lld

# Install target libc6-dev
RUN xx-apt-get install -y gcc libc6-dev

WORKDIR /app

COPY Cargo.toml Cargo.lock ./
COPY crates ./crates
COPY src ./src
COPY migrations ./migrations
COPY tests ./tests

# Use xx-cargo to automatically set target triple based on TARGETPLATFORM
RUN xx-cargo build --release

# Copy to a predictable location
RUN cp target/$(xx-cargo --print-target-triple)/release/imap-cache-rs ./imap-cache-rs

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/imap-cache-rs /app/imap-cache-rs

EXPOSE 1143 1993 8080

CMD ["/app/imap-cache-rs"]
