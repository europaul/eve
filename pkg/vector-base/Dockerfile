ARG VECTOR_FEATURES='--no-default-features --features sinks-opentelemetry,sources-syslog,unix,sources-file,sources-internal_metrics,sources-host_metrics,transforms-remap'
ARG RUST_VERSION=lfedge/eve-rust:1.84.1
FROM --platform=$BUILDPLATFORM ${RUST_VERSION} AS toolchain-base
ARG TARGETARCH
RUN apk update && apk add --no-cache git perl protoc

FROM toolchain-base AS target-amd64
ENV CARGO_BUILD_TARGET="x86_64-unknown-linux-musl"

FROM toolchain-base AS target-arm64
ENV CARGO_BUILD_TARGET="aarch64-unknown-linux-musl"

FROM toolchain-base AS target-riscv64
ENV CARGO_BUILD_TARGET="riscv64gc-unknown-linux-gnu"

FROM target-$TARGETARCH AS toolchain
RUN echo "Cargo target: $CARGO_BUILD_TARGET"

FROM toolchain AS planer
ADD https://github.com/vectordotdev/vector.git#v0.47.0 /app

WORKDIR /app
# create a recipe
RUN cargo chef prepare --recipe-path recipe.json

FROM toolchain AS cacher
ARG VECTOR_FEATURES
# copy the recipe
WORKDIR /app
COPY --from=planer /app/recipe.json recipe.json
# build the dependencies
RUN cargo chef cook --release $VECTOR_FEATURES --recipe-path recipe.json

# building the final image
FROM toolchain AS builder
ARG VECTOR_FEATURES
ADD https://github.com/vectordotdev/vector.git#v0.47.0 /app

WORKDIR /app

# copy prebuilt dependencies
# and the cargo directory with crates.io index
COPY --from=cacher /app/target /app/target
COPY --from=cacher $CARGO_HOME $CARGO_HOME
RUN echo "Cargo target: $CARGO_BUILD_TARGET"

RUN cargo build --release $VECTOR_FEATURES
RUN cargo sbom > sbom.spdx.json
RUN cp /app/target/$CARGO_BUILD_TARGET/release/vector /app/target/

FROM alpine:3.21 AS runtime
COPY --from=builder /app/target/vector /usr/bin/vector
COPY --from=builder /app/target/sbom.spdx.json /sbom.spdx.json
