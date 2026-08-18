# syntax=docker/dockerfile:1.7

ARG UBUNTU_VERSION=24.04
ARG GO_VERSION=1.25.5
ARG GPAC_REF=v26.07.0

# GPAC is compiled inside the target architecture. Buildx uses QEMU when the
# build host differs from TARGETPLATFORM, so configure sees the correct CPU.
FROM --platform=$TARGETPLATFORM ubuntu:${UBUNTU_VERSION} AS gpac-builder
ARG GPAC_REF
ENV DEBIAN_FRONTEND=noninteractive
WORKDIR /src
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config g++ git cmake yasm nasm autoconf automake libtool ca-certificates \
      zlib1g-dev libfreetype6-dev libjpeg-dev libpng-dev libmad0-dev libfaad-dev \
      libogg-dev libvorbis-dev libtheora-dev liba52-0.7.4-dev \
      libavcodec-dev libavformat-dev libavutil-dev libswscale-dev libavdevice-dev libavfilter-dev \
      libnghttp2-dev libopenjp2-7-dev libcaca-dev libxv-dev libgl1-mesa-dev libglu1-mesa-dev \
      libxvidcore-dev libssl-dev libjack-jackd2-dev libasound2-dev libpulse-dev libsdl2-dev \
      libcurl4-openssl-dev libxml2-dev libvpx-dev libopus-dev libass-dev
RUN git clone --depth 1 --branch "${GPAC_REF}" https://github.com/gpac/gpac.git /src/gpac
WORKDIR /src/gpac
RUN ./configure --prefix=/opt/gpac && \
    make -j"$(nproc)" && \
    make install && \
    LD_LIBRARY_PATH=/opt/gpac/lib /opt/gpac/bin/MP4Box -version && \
    LD_LIBRARY_PATH=/opt/gpac/lib /opt/gpac/bin/gpac -p=0 -h ffdmx >/dev/null && \
    LD_LIBRARY_PATH=/opt/gpac/lib /opt/gpac/bin/gpac -p=0 -h ffenc:* >/dev/null

# The release ZIP only contains target binaries/rootfs, so it can be fetched on
# BUILDPLATFORM without executing foreign-architecture code.
FROM --platform=$BUILDPLATFORM ubuntu:${UBUNTU_VERSION} AS wrapper-fetcher
ARG TARGETARCH
ARG WRAPPER_ARM64_URL=https://github.com/WorldObservationLog/wrapper/releases/download/wrapper.arm64.latest/Wrapper.arm64.latest.zip
ARG WRAPPER_AMD64_URL=https://github.com/WorldObservationLog/wrapper/releases/download/wrapper.x86_64.latest/Wrapper.x86_64.latest.zip
ARG WRAPPER_SHA256=""
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/wrapper
RUN case "${TARGETARCH}" in \
      arm64) wrapper_url="${WRAPPER_ARM64_URL}" ;; \
      amd64) wrapper_url="${WRAPPER_AMD64_URL}" ;; \
      *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl --fail --location --retry 4 --output /tmp/wrapper.zip "${wrapper_url}" && \
    if [ -n "${WRAPPER_SHA256}" ]; then echo "${WRAPPER_SHA256}  /tmp/wrapper.zip" | sha256sum -c -; fi && \
    unzip -q /tmp/wrapper.zip -d /opt/wrapper && \
    rm /tmp/wrapper.zip && \
    chmod +x /opt/wrapper/wrapper

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS go-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/apple-music-dl .

FROM --platform=$TARGETPLATFORM ubuntu:${UBUNTU_VERSION} AS runtime
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive \
    LD_LIBRARY_PATH=/opt/gpac/lib \
    AMDL_LISTEN=0.0.0.0:8080 \
    AMDL_QUEUE_DB=/app/data/queue.db \
    AMDL_DOWNLOAD_STAGING=/tmp/amdl-downloads \
    AMDL_WRAPPER_FILES_DIR=/opt/wrapper/rootfs/data/data/com.apple.android.music/files
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates tini curl unzip ffmpeg \
      zlib1g libfreetype6 libjpeg-dev libpng-dev libmad0 libfaad2 \
      libogg0 libvorbis0a libtheora0 liba52-0.7.4 \
      libnghttp2-14 libopenjp2-7 libcaca0 libxv1 libgl1 libglu1-mesa libxvidcore4 \
      libssl3t64 libjack-jackd2-0 libasound2t64 libpulse0 libsdl2-2.0-0 \
      libcurl4t64 libxml2 libvpx9 libopus0 libass9 && \
    rm -rf /var/lib/apt/lists/*

COPY --from=gpac-builder /opt/gpac /opt/gpac
COPY --from=wrapper-fetcher /opt/wrapper /opt/wrapper
COPY --from=go-builder /out/apple-music-dl /usr/local/bin/apple-music-dl
COPY docker/entrypoint.sh /usr/local/bin/container-entrypoint
COPY config.yaml.example /app/config.yaml
RUN chmod +x /usr/local/bin/container-entrypoint && \
    ln -s /opt/gpac/bin/gpac /usr/local/bin/gpac && \
    ln -s /opt/gpac/bin/MP4Box /usr/local/bin/MP4Box && \
    ln -s /opt/wrapper/wrapper /usr/local/bin/wrapper && \
    sed -i 's|^alac-save-folder:.*|alac-save-folder: /tmp/amdl-downloads/default/ALAC|' /app/config.yaml && \
    sed -i 's|^atmos-save-folder:.*|atmos-save-folder: /tmp/amdl-downloads/default/Atmos|' /app/config.yaml && \
    sed -i 's|^aac-save-folder:.*|aac-save-folder: /tmp/amdl-downloads/default/AAC|' /app/config.yaml && \
    sed -i 's|^mv-save-folder:.*|mv-save-folder: /tmp/amdl-downloads/default/Music Videos|' /app/config.yaml && \
    sed -i 's|^exit-on-error:.*|exit-on-error: true|' /app/config.yaml && \
    mkdir -p /tmp/amdl-downloads /app/data /opt/wrapper/rootfs/data/data/com.apple.android.music/files

WORKDIR /app
EXPOSE 8080 10020 20020 30020
HEALTHCHECK --interval=30s --timeout=4s --start-period=20s --retries=3 CMD curl -fsS http://127.0.0.1:8080/api/health || exit 1
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/container-entrypoint"]
CMD ["--web"]
