# Multi-architecture container

The image contains the Go web/API service, a target-native Wrapper release,
headless GPAC compiled from a stable tag, and a target-native FFmpeg build with
software decoding, transcoding, subtitles, OpenJPEG and audio analysis support.
Development packages and compiler output remain in builder stages only.

## Image layout

```text
ffmpeg-builder (linux/amd64 or linux/arm64)
  -> builds shared FFmpeg and validates codecs, subtitles and spectrum filters
gpac-builder (linux/amd64 or linux/arm64)
  -> builds headless GPAC against that FFmpeg and validates MP4Box + filters
wrapper-fetcher (build host)
  -> selects Wrapper.x86_64.latest or Wrapper.arm64.latest
go-builder (build host)
  -> cross-compiles the Go server and embeds frontend/dist
runtime (Ubuntu 24.04, target architecture)
  -> GPAC + FFmpeg + Wrapper + Go web service
```

## Build locally

Docker Desktop or a Linux Docker installation with Buildx is required.

```sh
chmod +x scripts/build-images.sh
./scripts/build-images.sh
```

This loads `apple-music-downloader:amd64` and
`apple-music-downloader:arm64` into the local Docker image store. The ARM build
uses QEMU when the build machine is amd64, so GPAC compilation is substantially
slower than a native build.

To publish one multi-platform manifest:

```sh
IMAGE=ghcr.io/owner/apple-music-downloader PUSH=1 ./scripts/build-images.sh
```

The publish path pushes two inspectable single-platform tags first:
`amd64` and `arm64`. It then creates the lightweight `latest` image index that
references both. A normal `docker pull ...:latest` still selects only the image
matching the host architecture; users may also pull an architecture tag
explicitly.

Override the pinned GPAC or FFmpeg release when upgrading:

```sh
GPAC_REF=v26.07.0 FFMPEG_VERSION=6.1.6 ./scripts/build-images.sh
```

The runtime image deliberately omits GPAC/FFmpeg headers, static libraries,
pkg-config files, build caches and desktop playback stacks (OpenGL, Mesa/LLVM,
SDL, Caca, Jack, Pulse and ALSA output). File demuxing/muxing, MP4Box metadata,
FFmpeg codec filters, subtitles, OpenJPEG and all downloader conversion formats
remain enabled. This is suitable for the container's server-side workload, but
the image is not intended to be a desktop media player or capture workstation.

## First run

Start the container first; Apple credentials are entered only in the local web
login dialog. Wrapper's Android runtime requires privileged container access.
Run it with `--privileged`, matching the upstream Wrapper deployment
instructions. Privileged mode is required by Wrapper's Android runtime, but it
does not by itself prevent the ARM64 Wrapper child from restarting during a
media request; the backend restores the device playlist context and retries
decryption without downloading the encrypted media again:

```sh
docker compose up -d --build
```

Without Compose, create the container directly:

```sh
docker volume create apple-music-downloader-queue
docker volume create apple-music-downloader-wrapper

docker run -d \
  --name apple-music-downloader \
  --restart unless-stopped \
  --privileged \
  -p 127.0.0.1:8080:8080 \
  --mount source=apple-music-downloader-queue,target=/app/data \
  --mount source=apple-music-downloader-wrapper,target=/opt/wrapper/rootfs/data/data/com.apple.android.music/files \
  apple-music-downloader:local
```

Open <http://localhost:8080>. The service is published only on the host loopback
address (`127.0.0.1`). Docker Compose mounts named volumes only; it does not bind
host directories, configuration files, or Docker Secrets.

Choose **下载目录** to let the browser handle completed files using its normal
download settings. Choose **其他位置** to grant a compatible browser a directory
handle for the current page. In both modes, files are streamed from the
container; the staged copy is removed after delivery or browser acknowledgement.

`apple-music-downloader-queue` stores `/app/data/queue.db`. Playlist submission
resolves every catalog page and writes one pending row per track, including its
display metadata and stable queue order. As soon as a task starts it is deleted
from SQLite. Progress, completed history, and failed history remain in memory
and disappear when the process is recreated.

`apple-music-downloader-wrapper` mounts Wrapper's complete Apple Music `files`
directory. This persists `kvs.sqlitedb`, its WAL files, cookies, account data,
tokens, and the other files Wrapper needs to reuse an authenticated session.
The volume is sensitive local account state. Completed downloads and temporary
2FA files are not retained as queue data; completed media staging stays under
`/tmp` and is removed after browser delivery.

If no usable Wrapper process is running, the page opens a blocking login dialog.
Credentials are accepted by `POST /api/wrapper/login`, used only to start the
upstream Wrapper login command, and are not written to SQLite, configuration,
browser storage, or logs. After Wrapper creates its account database, the
credential-bearing process is stopped and Wrapper is restarted without login
arguments. Upstream Wrapper only accepts credentials through `-L`, so they are
briefly visible in that child process command line inside the container while
the first login is in progress.

### Two-factor authentication

Wrapper 1.2.0 waits up to 60 seconds for a six-digit Apple verification code
in this runtime file:

```text
/opt/wrapper/rootfs/data/data/com.apple.android.music/files/2fa.txt
```

The login dialog can submit that code while Wrapper is waiting. The local
API is `POST /api/wrapper/2fa` with JSON such as `{"code":"123456"}`. It
validates exactly six digits, writes the file with mode `0600`, and removes a
still-unconsumed code after 65 seconds. The code is never stored in SQLite.

2FA is intentionally accepted only from the local login dialog. The dialog
stays open while Wrapper reports progress, changes to the verification-code step
only when Apple requests 2FA, and closes after the credential-free Wrapper
process is ready. There is no Compose, environment-variable, or Docker Secret
input for 2FA.

Wrapper prints Apple protocol-dialog information, but does not expose an API for
selecting a trusted device, phone number, SMS, or voice-call destination. The
frontend therefore tells the user to check a trusted-device notification or the
trusted phone number and accepts only the final six digits Wrapper supports.

### Browser file delivery

The task response contains opaque file IDs, relative paths, and sizes; it never
exposes container filesystem paths. The frontend downloads each completed file
from `GET /api/tasks/{taskID}/files/{fileID}`. Browser-managed Downloads complete
in that response; custom-directory delivery acknowledges a successful write
with `DELETE` on the same URL. File System Access currently requires a
compatible local Chromium browser or a desktop app WebView with the same API.

## Important build inputs

- `GPAC_REF`: stable GPAC Git tag. It is pinned so builds remain reproducible.
- `FFMPEG_VERSION`: FFmpeg release compiled for each target architecture.
- `WRAPPER_ARM64_URL` and `WRAPPER_AMD64_URL`: mutable `latest` release assets.
- `WRAPPER_SHA256`: optional checksum for controlled/reproducible builds.
- `WRAPPER_DISABLED=1`: starts only the web/Go service for UI or API testing.

The Wrapper release tags are mutable. For production, mirror the chosen ZIP or
pass its current SHA-256 using `--build-arg WRAPPER_SHA256=...`.
