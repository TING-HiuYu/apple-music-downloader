# Apple Music Downloader

简体中文 · [English](README.md)

一个本地优先的 Apple Music 下载管理器：使用 Go 后端、内嵌 Web 前端、SQLite 队列，
并提供 `linux/amd64` 与 `linux/arm64` 多架构 Docker 镜像。

> [!IMPORTANT]
> 本项目仅用于个人研究、互操作性分析，以及对你有权访问内容的合法备份。使用时需要
> 有效的 Apple Music 订阅，并应遵守所在地法律及 Apple 服务条款。本项目与 Apple Inc.
> 无隶属、合作或背书关系。

## 本分支增加了什么

- 参考 [wenfeng110402/AppleMusic-Downloader](https://github.com/wenfeng110402/AppleMusic-Downloader)
  设计的本地 Web 界面。
- 原生 Go HTTP API：任务直接调用已有的 Go 下载函数，不把 CLI 当作子进程运行。
- 使用 SQLite 保存待执行任务，当前按顺序一次执行一个任务。
- 下载结果通过浏览器传给用户，因此容器不需要挂载宿主机下载目录。
- 在前端完成 Wrapper 登录、Apple 双重验证最终六位码输入、状态反馈和登出；本应用
  不持久化账号密码或验证码。
- 支持 ALAC、Dolby Atmos、AAC，以及使用 FFmpeg 转换为 FLAC、MP3、Opus、WAV。
- Ubuntu 24.04 的 AMD64/ARM64 镜像，分别内置对应架构的 Wrapper、原生编译的
  GPAC、FFmpeg、字幕支持、OpenJPEG 和相关媒体库。

原有命令行模式仍然保留；Docker 镜像默认启动 Web 模式。

## 项目设计

```text
本地浏览器
  ├─ 内嵌的 HTML/CSS/JavaScript
  ├─ 登录与最终六位验证码输入
  └─ 浏览器“下载目录”或用户选择的“其他位置”
          ↕ 仅通过宿主机回环地址访问
Go Web/API 服务
  ├─ Wrapper 生命周期与状态管理
  ├─ SQLite 待执行队列（串行执行）
  ├─ 项目已有的 Go 下载函数
  └─ 已完成文件的临时中转区
          ├─ Wrapper：Apple Music 登录与解密服务
          ├─ GPAC/MP4Box：媒体处理和封装
          └─ FFmpeg：解码、转码、字幕和编解码器
```

上游 CLI 的下载逻辑包含包级共享状态，因此 Web 任务有意采用串行执行。任务开始执行
时就会从 SQLite 中删除；执行进度、成功记录和失败记录只保存在内存中，SQLite 不用于
缓存进度。

完成的文件只在容器临时目录中保留到浏览器接收为止。API 使用不透明文件 ID，不会把
容器内路径暴露给前端。

## 使用要求

- Docker Desktop，或支持 Buildx 的 Docker Engine
- 有效的 Apple Music 订阅
- 现代浏览器；需要选择自定义目录时推荐 Chrome/Edge
- 能够为上游 Wrapper Android 运行时启用特权容器

Web 端口只发布到 `127.0.0.1`。请勿把它部署到公网：它的设计目标是可信的本地浏览器
或桌面 WebView，而不是多用户互联网服务。

## 安装与部署

### Docker Desktop / Docker 命令行（推荐）

不需要 Compose，也不需要挂载任何宿主机目录：

```sh
docker pull ghcr.io/ting-hiuyu/apple-music-downloader:latest

docker run -d \
  --name apple-music-downloader \
  --restart unless-stopped \
  --privileged \
  -p 127.0.0.1:8080:8080 \
  ghcr.io/ting-hiuyu/apple-music-downloader:latest
```

打开 <http://localhost:8080>。

使用 Docker Desktop 时，只要 Docker Desktop 正在运行，就可以在终端执行同一条命令。
`--privileged` 是必需的，因为 Wrapper 会启动类似 Android 的运行时并执行挂载；缺少
该参数时通常会看到 `mount /dev/urandom failed: Operation not permitted`。

日志显示 `0.0.0.0:8080` 指的是容器内部监听地址；宿主机上的
`-p 127.0.0.1:8080:8080` 仍然保证只有本机回环接口可以访问。

常用命令：

```sh
docker logs -f apple-music-downloader
docker restart apple-music-downloader
docker stop apple-music-downloader
docker rm apple-music-downloader
```

由于默认部署有意不创建 volume，删除并重新创建容器时，待执行 SQLite 队列和 Wrapper
会话也会一并删除。

### Docker Compose

构建当前代码并启动：

```sh
docker compose up -d --build
```

修改宿主机端口：

```sh
AMDL_PORT=8088 docker compose up -d --build
```

项目提供的 Compose 文件同样只绑定 `127.0.0.1`、启用特权模式，而且不挂载宿主机
目录、命名卷或 Docker Secrets。

### 本地构建多架构镜像

使用 Buildx 构建并加载两个架构：

```sh
chmod +x scripts/build-images.sh
./scripts/build-images.sh
```

构建结果为 `apple-music-downloader:amd64` 和 `apple-music-downloader:arm64`。
跨架构构建 GPAC 会使用 QEMU，因此可能比原生架构构建慢很多。

发布多平台 manifest：

```sh
IMAGE=ghcr.io/owner/apple-music-downloader PUSH=1 ./scripts/build-images.sh
```

发布时会先分别生成单架构的 `:amd64` 和 `:arm64` 镜像，再创建同时指向两者的
多架构 `:latest` 索引。普通用户拉取 `:latest` 时，Docker 仍然只会自动下载与宿主机
匹配的那一个架构。

推送到 `main` 时，项目内置 GitHub Actions 也会把两个单架构标签、不可变的
commit-架构标签，以及自动识别架构的 `latest` 和 commit SHA 索引发布到 GHCR。

### 从 Go 源码运行

`frontend/dist` 中的前端会被编译进 Go 二进制：

```sh
cp config.yaml.example config.yaml
go run . --web --listen 127.0.0.1:8080
```

这会启动 Web UI/API，但实际解密仍需要可访问的 Wrapper 和本机媒体工具。Docker 是
目前推荐的一体化安装方式。

## 第一次使用

1. 打开本地页面。如果 Wrapper 没有可用会话，登录弹窗会自动出现。
2. 输入 Apple Music 账号凭据。它们只会发送到本地 Go 服务，用来启动 Wrapper 登录。
3. 如果 Apple 要求双重验证，请检查受信任设备通知或绑定手机号收到的短信，然后在
   同一弹窗中输入最终六位验证码。
4. 添加 Apple Music 链接，选择 ALAC/Atmos/AAC 和可选的转换格式，再提交任务。
5. 选择“下载目录”时由浏览器接管下载；选择“其他位置”时，可在兼容浏览器中指定目录。

Wrapper 能接收最终验证码，但没有提供让本项目选择 Apple 验证码发送方式、受信任设备
或手机号的接口。

下载专辑时，如果使用浏览器“下载目录”，浏览器可能请求“允许下载多个文件”的权限，
请按需允许。

## 数据与账号凭据

- 账号密码不会写入 SQLite、配置文件、浏览器存储或应用日志。
- 上游 Wrapper 的登录接口通过命令行参数接收凭据。因此在首次登录期间，拥有容器内
  足够权限的进程可能短暂看到这些参数。认证完成后，后端会停止该进程，并在不携带
  登录参数的情况下重新启动 Wrapper。
- 六位验证码只会以 `0600` 权限写入 Wrapper 要求的运行时文件，不会存入 SQLite。
- SQLite 只保存待执行队列，不缓存进度。
- 默认 Docker 部署没有 volume；会话、队列和中转文件都位于容器可写层。
- “下载目录”由浏览器自己的下载设置决定；“其他位置”由浏览器授权页面获取目录句柄，
  再把每个流式文件写入该目录。

## 构建参数

| 参数 | 用途 | 默认值 |
| --- | --- | --- |
| `GPAC_REF` | 为各目标架构编译的 GPAC stable Git 标签 | `v26.07.0` |
| `FFMPEG_VERSION` | 为各目标架构源码编译的 FFmpeg 版本 | `6.1.6` |
| `WRAPPER_ARM64_URL` | ARM64 Wrapper ZIP | `Wrapper.arm64.latest` |
| `WRAPPER_AMD64_URL` | AMD64 Wrapper ZIP | `Wrapper.x86_64.latest` |
| `WRAPPER_SHA256` | 可选的 Wrapper 文件校验 | 空 |
| `AMDL_PORT` | Compose 发布到宿主机的端口 | `8080` |
| `WRAPPER_DISABLED=1` | 开发 UI/API 时禁用 Wrapper | `0` |

Wrapper 的 `latest` release 标签会变化。需要可复现构建时，请镜像保存已知 ZIP，或通过
`WRAPPER_SHA256` 提供当前文件的 SHA-256。

运行镜像只保留 strip 后的 FFmpeg/GPAC 动态二进制和运行库；头文件、静态库、
pkg-config 文件与编译缓存均不会进入最终镜像。GPAC 的桌面播放和设备输出模块已关闭，
但 MP4Box、容器读写、编解码器、字幕、OpenJPEG、FLAC/MP3/Opus/WAV 转换以及 FFmpeg
分析滤镜仍然可用。

更多镜像细节见 [DOCKER-BUILD.md](DOCKER-BUILD.md)。

## 常见问题

### 没有收到双重验证码

先等前端明确进入六位验证码步骤。验证码由 Apple 发送，请检查受信任设备和绑定手机。
Wrapper 无法代替前端指定发送到哪台设备或哪个手机号。

### Wrapper 登录或解密出现挂载/权限错误

确认当前容器在创建时就带有 `--privileged`。给一条新的启动命令加参数不会修改已经创建
的容器，需要删除旧容器后重新创建。

### 页面无法打开

```sh
docker ps --filter name=apple-music-downloader
docker logs --tail 200 apple-music-downloader
curl http://127.0.0.1:8080/api/health
```

### 文件没有出现在预期目录

使用“下载目录”时，目标位置完全由浏览器管理，浏览器也可能重命名文件或请求多文件
下载权限。需要选择并显示明确目录时，请在 Chrome/Edge 中使用“其他位置”。

## 引用、致谢

本仓库基于 [zhaarey/apple-music-downloader](https://github.com/zhaarey/apple-music-downloader)
扩展而来。感谢原作者和所有贡献者完成 Apple Music 元数据、下载、解密、标签写入等
核心工作。

容器和 Web 版本还使用或参考了以下优秀项目：

- [WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper)：Wrapper
  运行时及各架构 release。
- [GPAC](https://github.com/gpac/gpac)：MP4Box 和媒体处理能力。
- [FFmpeg](https://github.com/FFmpeg/FFmpeg)：解码与转码能力。
- [wenfeng110402/AppleMusic-Downloader](https://github.com/wenfeng110402/AppleMusic-Downloader)：
  Web 界面设计参考。
- [chocomint](https://github.com/chocomint)：上游项目注明的 ARM64 agent 工作；同时感谢
  上游项目中参与流式解密和相关研究的贡献者。

各依赖继续遵循其自身的许可证和版权声明，详情请查看 `go.mod`、容器构建文件及上述
上游仓库。
