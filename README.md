# Apple Music Downloader

[简体中文](README-CN.md) · English

A local-first Apple Music download manager built with Go, an embedded web UI,
SQLite queue persistence, and multi-architecture Docker images for
`linux/amd64` and `linux/arm64`.

> [!IMPORTANT]
> This project is intended for personal research, interoperability, and lawful
> backups of media you are authorized to access. You need a valid Apple Music
> subscription. Follow local law and Apple's terms of service. This project is
> not affiliated with or endorsed by Apple Inc.

## What this fork adds

- A responsive local web interface inspired by
  [wenfeng110402/AppleMusic-Downloader](https://github.com/wenfeng110402/AppleMusic-Downloader).
- A native Go HTTP API: jobs call the existing downloader functions directly;
  the backend does not spawn the CLI as a subprocess.
- A serial task queue backed by SQLite for pending jobs.
- Browser-based file delivery, so the container needs no host-directory mount.
- Wrapper login, Apple two-factor-code submission, status reporting, and logout
  from the web UI. Credentials and verification codes are never persisted by
  this application.
- ALAC, Dolby Atmos, and AAC downloads, plus optional FLAC, MP3, Opus, or WAV
  conversion through FFmpeg.
- Ubuntu 24.04 images for AMD64 and ARM64 containing the matching Wrapper build,
  a target-native GPAC build, FFmpeg, subtitle support, OpenJPEG, and related
  media libraries.

The original CLI remains available. The Docker image starts web mode by default.

## Design

```text
Local browser
  ├─ embedded HTML/CSS/JS
  ├─ login and final 2FA-code input
  └─ browser Downloads or a user-selected directory
          ↕ HTTP on host loopback only
Go web/API service
  ├─ Wrapper lifecycle and status manager
  ├─ SQLite pending-job queue (serial execution)
  ├─ existing native Go downloader functions
  └─ temporary completed-file staging
          ├─ Wrapper: Apple Music authentication/decryption service
          ├─ GPAC/MP4Box: media processing and packaging
          └─ FFmpeg: decoding, transcoding, subtitles, and codecs
```

The downloader uses package-level state inherited from the upstream CLI, so web
jobs are deliberately serialized. A job is removed from SQLite when execution
starts. Running progress and completed/failed history live in memory; SQLite is
not used as a progress cache.

Completed files stay in a temporary container directory only until the browser
has received them. The API exposes opaque file IDs rather than container paths.

## Requirements

- Docker Desktop, or Docker Engine with Buildx
- A valid Apple Music subscription
- A modern browser; Chrome/Edge is recommended for saving to a custom directory
- Privileged-container support for the upstream Wrapper Android runtime

The web port is published only to `127.0.0.1`. Do not expose it publicly: the
application is designed for a trusted local browser or desktop WebView, not as a
multi-user internet service.

## Install and run

### Docker Desktop / Docker CLI (recommended)

No Compose file and no host-directory mount are required:

```sh
docker pull ghcr.io/ting-hiuyu/apple-music-downloader:latest

docker run -d \
  --name apple-music-downloader \
  --restart unless-stopped \
  --privileged \
  -p 127.0.0.1:8080:8080 \
  ghcr.io/ting-hiuyu/apple-music-downloader:latest
```

Open <http://localhost:8080>.

On Docker Desktop, run the same command in a terminal while Docker Desktop is
running. `--privileged` is required because Wrapper starts an Android-like
runtime and performs mounts. Without it, errors such as
`mount /dev/urandom failed: Operation not permitted` are expected.

The log reports `0.0.0.0:8080` because that is the address inside the container.
The `-p 127.0.0.1:8080:8080` mapping still restricts host access to loopback.

Useful commands:

```sh
docker logs -f apple-music-downloader
docker restart apple-music-downloader
docker stop apple-music-downloader
docker rm apple-music-downloader
```

Removing/recreating the container also removes its pending SQLite queue and
Wrapper session because this deployment intentionally defines no volume.

### Docker Compose

Build the current checkout and start it:

```sh
docker compose up -d --build
```

Change the host port if needed:

```sh
AMDL_PORT=8088 docker compose up -d --build
```

The supplied Compose file also binds only to `127.0.0.1`, enables privileged
mode, and mounts no host paths or Docker Secrets.

### Build images locally

Build and load both target architectures with Buildx:

```sh
chmod +x scripts/build-images.sh
./scripts/build-images.sh
```

This creates `apple-music-downloader:amd64` and
`apple-music-downloader:arm64`. Cross-architecture GPAC compilation uses QEMU
and can take considerably longer than a native build.

To publish a multi-platform manifest:

```sh
IMAGE=ghcr.io/owner/apple-music-downloader PUSH=1 ./scripts/build-images.sh
```

Pushes to `main` also trigger the included GitHub Actions workflow, publishing
`linux/amd64` and `linux/arm64` images to GHCR with `latest` and commit-SHA tags.

### Run from Go source

The web assets in `frontend/dist` are embedded into the Go binary:

```sh
cp config.yaml.example config.yaml
go run . --web --listen 127.0.0.1:8080
```

This starts the UI/API, but successful decryption still requires a reachable
Wrapper instance and the media tools configured for the local platform. Docker
is the supported all-in-one installation.

## First-use workflow

1. Open the local page. If Wrapper has no usable session, the login dialog opens
   automatically.
2. Enter the Apple Music account credentials. They are sent to the local Go
   service only to start Wrapper's login flow.
3. If Apple requests two-factor authentication, check the trusted-device prompt
   or the SMS sent to the trusted phone number, then enter the final six-digit
   code in the same dialog.
4. Add an Apple Music URL, choose ALAC/Atmos/AAC and an optional conversion
   format, then submit the job.
5. Choose **Downloads** to let the browser handle the file, or **Other location**
   to select a directory in a compatible browser.

Wrapper accepts the final verification code but does not provide an API for this
project to select Apple's delivery method, trusted device, or phone number.

For an album, the browser may ask for permission to download multiple files.
Allow it if you selected browser Downloads.

## Data and credential handling

- Account credentials are not written to SQLite, config files, browser storage,
  or application logs.
- Wrapper's upstream login interface accepts credentials through command-line
  arguments. They may therefore be briefly visible to processes with sufficient
  access inside the privileged container while initial login is running. After
  authentication, the backend restarts Wrapper without those arguments.
- The six-digit verification code is written with mode `0600` only to Wrapper's
  expected runtime path and is not stored in SQLite.
- SQLite stores pending queue entries only; it does not cache progress.
- The default Docker deployment has no volumes. Session, queue, and staged files
  are part of the container's ephemeral writable layer.
- Browser Downloads use the browser's configured directory. With **Other
  location**, the browser grants the page a directory handle and writes each
  streamed file there.

## Build configuration

| Setting | Purpose | Default |
| --- | --- | --- |
| `GPAC_REF` | GPAC stable Git tag compiled for each target | `v26.07.0` |
| `WRAPPER_ARM64_URL` | ARM64 Wrapper release ZIP | `Wrapper.arm64.latest` |
| `WRAPPER_AMD64_URL` | AMD64 Wrapper release ZIP | `Wrapper.x86_64.latest` |
| `WRAPPER_SHA256` | Optional release checksum validation | empty |
| `AMDL_PORT` | Compose host port | `8080` |
| `WRAPPER_DISABLED=1` | Disable Wrapper for UI/API development | `0` |

Wrapper's `latest` release tags are mutable. For controlled builds, mirror a
known ZIP or pass its SHA-256 through `WRAPPER_SHA256`.

More image details are in [DOCKER-BUILD.md](DOCKER-BUILD.md).

## Troubleshooting

### No verification code arrives

Wait until the UI explicitly changes to the six-digit-code step. Delivery is
controlled by Apple: check trusted devices and the trusted phone number. Wrapper
cannot request a particular destination on behalf of the UI.

### Wrapper login or decryption fails with a mount/permission error

Confirm the existing container was created with `--privileged`. Adding the flag
to a new command does not change an already-created container; remove and create
that container again.

### The UI is not reachable

```sh
docker ps --filter name=apple-music-downloader
docker logs --tail 200 apple-music-downloader
curl http://127.0.0.1:8080/api/health
```

### Files do not appear in the expected directory

With **Downloads**, the browser owns the destination and may rename files or ask
for multiple-download permission. Use **Other location** in Chrome/Edge when you
need to select and display an explicit directory.

## Credits and thanks

This repository is a fork and extension of
[zhaarey/apple-music-downloader](https://github.com/zhaarey/apple-music-downloader).
Many thanks to its author and contributors for the Apple Music metadata,
download, decryption, and tagging foundation.

The container and web experience also rely on or draw inspiration from:

- [WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper)
  for the runtime and architecture-specific releases.
- [GPAC](https://github.com/gpac/gpac) for MP4Box and media processing.
- [FFmpeg](https://github.com/FFmpeg/FFmpeg) for decoding and transcoding.
- [wenfeng110402/AppleMusic-Downloader](https://github.com/wenfeng110402/AppleMusic-Downloader)
  for web-interface design inspiration.
- [chocomint](https://github.com/chocomint) for the ARM64 agent work credited by
  the upstream project, plus the upstream contributors credited for streaming
  decryption and related research.

Dependencies retain their own licenses and copyrights. See `go.mod`, the
container build files, and the linked upstream repositories for details.
