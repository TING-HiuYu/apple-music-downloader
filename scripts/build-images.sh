#!/usr/bin/env sh
set -eu

image="${IMAGE:-apple-music-downloader}"
gpac_ref="${GPAC_REF:-v26.07.0}"
ffmpeg_version="${FFMPEG_VERSION:-6.1.6}"
builder="${BUILDER:-amdl-multiarch}"
push="${PUSH:-0}"

if ! docker buildx inspect "${builder}" >/dev/null 2>&1; then
  docker buildx create --name "${builder}" --driver docker-container --use
else
  docker buildx use "${builder}"
fi
docker buildx inspect --bootstrap >/dev/null

if [ "${push}" = "1" ]; then
  docker buildx build \
    --platform linux/amd64 \
    --build-arg "GPAC_REF=${gpac_ref}" \
    --build-arg "FFMPEG_VERSION=${ffmpeg_version}" \
    --tag "${image}:amd64" \
    --push .

  docker buildx build \
    --platform linux/arm64 \
    --build-arg "GPAC_REF=${gpac_ref}" \
    --build-arg "FFMPEG_VERSION=${ffmpeg_version}" \
    --tag "${image}:arm64" \
    --push .

  docker buildx imagetools create \
    --tag "${image}:latest" \
    "${image}:amd64" \
    "${image}:arm64"

  echo "Published ${image}:amd64, ${image}:arm64 and multi-platform ${image}:latest"
  exit 0
fi

docker buildx build \
  --platform linux/amd64 \
  --build-arg "GPAC_REF=${gpac_ref}" \
  --build-arg "FFMPEG_VERSION=${ffmpeg_version}" \
  --tag "${image}:amd64" \
  --load .

docker buildx build \
  --platform linux/arm64 \
  --build-arg "GPAC_REF=${gpac_ref}" \
  --build-arg "FFMPEG_VERSION=${ffmpeg_version}" \
  --tag "${image}:arm64" \
  --load .

echo "Built ${image}:amd64 and ${image}:arm64"
