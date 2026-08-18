# syntax=docker/dockerfile:1.7

ARG UBUNTU_VERSION=24.04
ARG GO_VERSION=1.25.5
ARG GPAC_REF=v26.07.0
ARG FFMPEG_VERSION=6.1.6

# Build a server-oriented FFmpeg with the software codecs, muxers, filters,
# subtitle renderers and image formats used by the downloader. Desktop display,
# hardware/GPU and device-capture integrations are intentionally omitted.
FROM --platform=$TARGETPLATFORM ubuntu:${UBUNTU_VERSION} AS ffmpeg-builder
ARG FFMPEG_VERSION
ENV DEBIAN_FRONTEND=noninteractive
WORKDIR /src
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config git ca-certificates yasm nasm \
      zlib1g-dev libbz2-dev liblzma-dev libssl-dev \
      libaom-dev libass-dev libdav1d-dev libfontconfig1-dev libfreetype6-dev libfribidi-dev \
      libjxl-dev libmp3lame-dev libopenjp2-7-dev libopus-dev librav1e-dev libsoxr-dev \
      libtheora-dev libvorbis-dev libvpx-dev libwebp-dev \
      libx264-dev libx265-dev libxml2-dev libzimg-dev
RUN git clone --depth 1 --branch "n${FFMPEG_VERSION}" https://github.com/FFmpeg/FFmpeg.git /src/ffmpeg
WORKDIR /src/ffmpeg
RUN ./configure \
      --prefix=/opt/ffmpeg \
      --enable-shared \
      --disable-static \
      --disable-debug \
      --disable-doc \
      --disable-ffplay \
      --disable-autodetect \
      --enable-gpl \
      --enable-version3 \
      --enable-openssl \
      --enable-zlib \
      --enable-bzlib \
      --enable-lzma \
      --enable-libass \
      --enable-libfontconfig \
      --enable-libfreetype \
      --enable-libfribidi \
      --enable-libaom \
      --enable-libdav1d \
      --enable-libjxl \
      --enable-libmp3lame \
      --enable-libopenjpeg \
      --enable-libopus \
      --enable-librav1e \
      --enable-libsoxr \
      --enable-libtheora \
      --enable-libvorbis \
      --enable-libvpx \
      --enable-libwebp \
      --enable-libx264 \
      --enable-libx265 \
      --enable-libxml2 \
      --enable-libzimg \
      --disable-alsa \
      --disable-audiotoolbox \
      --disable-cuda \
      --disable-cuvid \
      --disable-d3d11va \
      --disable-dxva2 \
      --disable-ffnvcodec \
      --disable-libdrm \
      --disable-mediafoundation \
      --disable-nvdec \
      --disable-nvenc \
      --disable-openal \
      --disable-opencl \
      --disable-opengl \
      --disable-sdl2 \
      --disable-v4l2-m2m \
      --disable-vaapi \
      --disable-vdpau \
      --disable-videotoolbox \
      --disable-vulkan \
      --disable-xlib && \
    make -j"$(nproc)" && \
    make install && \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib /opt/ffmpeg/bin/ffmpeg -hide_banner -version && \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib /opt/ffmpeg/bin/ffprobe -hide_banner -version && \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib /opt/ffmpeg/bin/ffmpeg -hide_banner -filters 2>/dev/null | grep -q showspectrumpic && \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib /opt/ffmpeg/bin/ffmpeg -hide_banner -encoders 2>/dev/null | grep -q ' flac ' && \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib /opt/ffmpeg/bin/ffmpeg -hide_banner -encoders 2>/dev/null | grep -q libmp3lame && \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib /opt/ffmpeg/bin/ffmpeg -hide_banner -encoders 2>/dev/null | grep -q libopus && \
    ! LD_LIBRARY_PATH=/opt/ffmpeg/lib ldd /opt/ffmpeg/bin/ffmpeg | \
      grep -E 'lib(GL|GLU|SDL|placebo|OpenCL|vulkan|drm|caca|jack|asound|pulse)'

# GPAC is compiled inside the target architecture against the custom FFmpeg.
# Its file, subtitle, packaging and codec filters stay enabled; only local
# playback/rendering and device output modules are disabled.
FROM ffmpeg-builder AS gpac-builder
ARG GPAC_REF
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      zlib1g-dev libfreetype6-dev libjpeg-dev libpng-dev libmad0-dev libfaad-dev \
      libogg-dev libvorbis-dev libtheora-dev liba52-0.7.4-dev \
      libnghttp2-dev libopenjp2-7-dev libxvidcore-dev libssl-dev \
      libcurl4-openssl-dev libxml2-dev libvpx-dev libopus-dev libass-dev
RUN git clone --depth 1 --branch "${GPAC_REF}" https://github.com/gpac/gpac.git /src/gpac
WORKDIR /src/gpac
ENV PKG_CONFIG_PATH=/opt/ffmpeg/lib/pkgconfig \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib
RUN ./configure \
      --prefix=/opt/gpac \
      --disable-3d \
      --disable-compositor \
      --disable-vout \
      --disable-aout \
      --disable-avdevice \
      --disable-qjs \
      --disable-resample \
      --disable-x11 \
      --disable-x11-shm \
      --disable-x11-xv \
      --use-alsa=no \
      --use-pulseaudio=no \
      --use-jack=no \
      --use-libcaca=no \
      --use-sdl=no \
      --use-ffmpeg=/opt/ffmpeg && \
    make -j"$(nproc)" && \
    make install && \
    LD_LIBRARY_PATH=/opt/gpac/lib:/opt/ffmpeg/lib /opt/gpac/bin/MP4Box -version && \
    LD_LIBRARY_PATH=/opt/gpac/lib:/opt/ffmpeg/lib /opt/gpac/bin/gpac -p=0 -h ffdmx >/dev/null && \
    LD_LIBRARY_PATH=/opt/gpac/lib:/opt/ffmpeg/lib /opt/gpac/bin/gpac -p=0 -h ffenc:* >/dev/null && \
    ! ldd /opt/gpac/lib/libgpac.so | grep -E 'lib(GL|GLU|SDL|placebo|OpenCL|vulkan|drm|caca|jack|asound|pulse)' && \
    (find /opt/gpac/bin /opt/gpac/lib /opt/ffmpeg/bin /opt/ffmpeg/lib \
      -type f -exec strip --strip-unneeded {} + 2>/dev/null || true) && \
    rm -rf \
      /opt/gpac/include /opt/gpac/lib/pkgconfig /opt/gpac/lib/libgpac_static.a \
      /opt/gpac/share/doc /opt/gpac/share/man \
      /opt/ffmpeg/include /opt/ffmpeg/lib/pkgconfig /opt/ffmpeg/share

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
    LD_LIBRARY_PATH=/opt/gpac/lib:/opt/ffmpeg/lib \
    AMDL_LISTEN=0.0.0.0:8080 \
    AMDL_QUEUE_DB=/app/data/queue.db \
    AMDL_DOWNLOAD_STAGING=/tmp/amdl-downloads \
    AMDL_WRAPPER_FILES_DIR=/opt/wrapper/rootfs/data/data/com.apple.android.music/files
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates tini curl unzip \
      zlib1g libbz2-1.0 liblzma5 libssl3t64 \
      libaom3 libass9 libdav1d7 libfontconfig1 libfreetype6 libfribidi0 \
      libjxl0.7 libmp3lame0 libopenjp2-7 libopus0 librav1e0 libsoxr0 \
      libtheora0 libvorbis0a libvorbisenc2 libvpx9 \
      libwebp7 libwebpmux3 libx264-164 libx265-199 libxml2 libzimg2 \
      libjpeg-turbo8 libpng16-16t64 libmad0 libfaad2 \
      libogg0 liba52-0.7.4 \
      libnghttp2-14 libxvidcore4 libcurl4t64 && \
    rm -rf /var/lib/apt/lists/*

COPY --from=gpac-builder /opt/gpac /opt/gpac
COPY --from=gpac-builder /opt/ffmpeg /opt/ffmpeg
COPY --from=wrapper-fetcher /opt/wrapper /opt/wrapper
COPY --from=go-builder /out/apple-music-dl /usr/local/bin/apple-music-dl
COPY docker/entrypoint.sh /usr/local/bin/container-entrypoint
COPY config.yaml.example /app/config.yaml
RUN chmod +x /usr/local/bin/container-entrypoint && \
    ln -s /opt/gpac/bin/gpac /usr/local/bin/gpac && \
    ln -s /opt/gpac/bin/MP4Box /usr/local/bin/MP4Box && \
    ln -s /opt/ffmpeg/bin/ffmpeg /usr/local/bin/ffmpeg && \
    ln -s /opt/ffmpeg/bin/ffprobe /usr/local/bin/ffprobe && \
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
