# M3U8 下载器

一个在本机运行的 Go Web 应用，用于下载 HLS/M3U8 媒体并合并为 MP4。

## 功能

- 支持自定义 Referer、Cookie 和 User-Agent。
- Go 负责解析播放清单并并发下载分片；FFmpeg 只读取本地缓存播放清单并合并 MP4。
- 支持 `1`、`4`、`8`、`16` 个分片 worker，默认 `8`；支持主/媒体清单、TS、fMP4、AES-128、初始化分片和字节范围点播。
- 任务列表实时显示下载、合并、暂停、继续和取消状态；点击任务会进入独立详情页，每秒增量同步运行日志。
- 使用 SQLite 持久化任务、运行日志和默认下载设置；服务重启后仍可查看历史任务。
- 保存目录与缓存目录既可手动输入，也可通过 Windows 系统目录选择器选择。
- 可选在合并成功后删除临时缓存；停止或失败任务的缓存默认保留 `168` 小时，可在设置页调整为 `0` 以永久保留。重新提交相同来源和输出名时会自动复用已完成分片。
- 服务重启后会自动继续排队或下载中的任务，并复用完整缓存分片；重启前处于暂停状态的任务会保持暂停，需手动继续。
- 内置本地管理员登录、退出和密码修改；密码仅以 bcrypt 散列形式保存在 SQLite。
- 支持绑定本地 Telegram Bot API Server，多个私聊、群组或频道 Chat ID 可接收视频；Bot 以交互式步骤收集 M3U8 地址、Referer、Cookie、输出名、分片并发数和缓存策略，并提供任务列表、进度、暂停、继续、停止和上传按钮。任务详情的“刷新”与自动进度更新会编辑原消息，不会重复发送新消息。
- 完成视频可手动或自动上传 Telegram；超过设置大小时会由 FFmpeg 切分为可独立播放的 MP4 文件段，并按每组 `2` 至 `10` 段作为 Telegram 视频相册上传，默认每段 `1900 MB`。相册总量超过 Bot API 单次 HTTP 请求安全大小时，会逐段暂存并复用 `file_id` 创建同一个相册，随后删除暂存消息；上传完成后自动清理临时分段。

## 前置条件

- Go `1.26` 或更高版本。
- FFmpeg 已安装，或可通过绝对路径指定其可执行文件。

## 运行

```powershell
go run . -ffmpeg-path H:/video/ffmpeg.exe
```

默认访问地址为 `http://127.0.0.1:8080`。可通过环境变量 `ADDR` 修改监听地址：

```powershell
$env:ADDR = '127.0.0.1:8081'
go run . -ffmpeg-path H:/video/ffmpeg.exe
```

也可以使用环境变量配置 FFmpeg：

```powershell
$env:FFMPEG_PATH = 'H:/video/ffmpeg.exe'
go run .
```

首次启动会创建 `admin` 账号。若未设置 `M3U8_ADMIN_PASSWORD`，程序会在本机控制台打印一次随机临时密码；登录后请立即通过页头“修改密码”更新。也可以在首次启动前直接指定密码：

```powershell
$env:M3U8_ADMIN_PASSWORD = '请设置至少8位的强密码'
go run . -ffmpeg-path H:/video/ffmpeg.exe
```

SQLite 数据库默认保存在 `data/m3u8-downloader.db`，首次启动时会将默认保存目录、缓存目录、缓存清理策略和分片并发数写入数据库，可在主页“默认设置”中修改。

忘记管理员密码时，先停止正在运行的服务，再在运行程序的本机交互式终端执行以下命令。该命令会隐藏密码输入、要求重复确认，并且只重置本地 `admin` 账号，不启动 Web 服务：

```powershell
.\m3u8-downloader.exe -reset-password
```

## Telegram Bot

下载器使用官方本地 Bot API Server 以支持本地文件路径和最高 `2000 MB` 的上传。项目提供 Windows 安装脚本；首次执行会拉取官方源码、vcpkg 与构建依赖，耗时较长并会占用较多磁盘空间：

```powershell
.\scripts\install-telegram-bot-api.ps1 -InstallPrerequisites
```

安装器需要 Git、CMake 和 Visual Studio 2022 Build Tools（C++ 桌面开发工作负载）。`-InstallPrerequisites` 会通过 `winget` 安装缺失的前置条件；若刚安装 Git 或 CMake，请重新打开 PowerShell 后再次执行脚本。安装完成后的程序路径为 `tools/telegram-bot-api/telegram-bot-api.exe`。

启动本地服务时必须提供你自己的 Telegram API ID 和 API Hash，并启用 `--local`：

```powershell
& '.\tools\telegram-bot-api\telegram-bot-api.exe' --api-id '你的 API ID' --api-hash '你的 API Hash' --local --http-port 8081 --dir '.\data\telegram-bot-api'
```

默认服务地址为 `http://127.0.0.1:8081`。登录下载器后，在主页的“Telegram Bot”区域填写 Bot Token、一个或多个目标 Chat ID、自动上传选项和切分大小。多个 Chat ID 以英文逗号分隔；私聊、群组和频道均可作为发送目标。Bot 只处理已绑定 Chat ID 的 `/start`、`/tasks`、`/completed` 和内联按钮操作。

请仅下载你拥有访问和保存权限的媒体内容。

## GitHub 发布

仓库提供三个仅可在 Actions 页面手动运行的工作流：

- “构建多系统发布包”：只需填写一个源码分支或 tag；它会构建 Ubuntu latest、Debian 13、Alpine latest 和 Windows latest 的 AMD64 压缩包。选择分支时只上传 Actions Artifact，不创建任何 tag 或 Release；选择仓库中已有的 tag 时才创建或更新同名 GitHub Release。
- “编译单系统包”：选择一个系统，只编译并上传该系统的工作流产物，不创建 Release。
- “发布 Docker 镜像”：默认读取最新 GitHub Release tag 的 `m3u8-downloader-alpine-latest-amd64.tar.gz`，不在镜像构建阶段重新编译。镜像基于 Alpine latest，内置 FFmpeg、FFprobe、CA 证书和时区数据；上传至 `ghcr.io/smagicalk/m3u8_downlaod`，同时标记该发布 tag 和 `latest`。

首次推送镜像前，请在仓库 Actions 设置中允许工作流拥有 `Read and write permissions`，并确认 GitHub Packages 对仓库可写。可直接启动镜像：

```bash
docker run -d --name m3u8-downloader --restart unless-stopped \
  -p 8080:8080 \
  -e TZ=Asia/Shanghai \
  -e M3U8_ADMIN_PASSWORD='请设置至少8位的强密码' \
  -e TELEGRAM_BOT_API_URL='http://telegram-bot-api:8081' \
  -v m3u8-data:/app/data \
  -v m3u8-downloads:/app/downloads \
  -v m3u8-cache:/app/cache \
  ghcr.io/smagicalk/m3u8_downlaod:latest
```

下载器镜像本身不包含 Telegram Bot API Server。单独使用 `docker run` 时，请另行部署 Bot API，并确保两个容器位于同一个 Docker 网络。`TELEGRAM_BOT_API_URL` 的镜像默认值为 `http://telegram-bot-api:8081`，可按 Bot API 容器的服务名覆盖；该变量只用于首次初始化，网页中已保存的地址优先。不要填写下载器容器自身的 `127.0.0.1`。

推荐直接使用仓库中的 Compose 配置，它会同时启动 `evilfreelancer/docker-telegram-bot-api:latest`。先在项目目录创建 `.env` 并填写从 Telegram 获取的应用凭据，不要将该文件提交到 Git：

```dotenv
TELEGRAM_API_ID=你的_API_ID
TELEGRAM_API_HASH=你的_API_HASH
M3U8_ADMIN_PASSWORD=请设置至少8位的强密码
```

然后启动服务：

```bash
docker compose pull
docker compose up -d
docker compose logs m3u8-downloader
```

Compose 默认创建 `m3u8-network` 网络、下载器的三个持久化卷和 Bot API 数据卷，并使用 `http://telegram-bot-api:8081` 作为首次 Bot API 地址。下载卷和缓存卷会以只读方式挂载到 Bot API 容器的相同路径，以支持本地文件上传。Bot API 的宿主机端口默认只监听 `127.0.0.1:8081`。

未设置 `M3U8_ADMIN_PASSWORD` 时，首次登录临时密码会显示在 `docker compose logs m3u8-downloader` 中。端口、镜像 Tag、网络、密码和 Bot API 地址均可通过同名环境变量或 `.env` 文件覆盖；可通过 `TELEGRAM_BOT_API_BIND`、`TELEGRAM_BOT_API_PORT` 和 `TELEGRAM_BOT_API_DATA_VOLUME` 调整 Bot API 的监听地址、端口及数据卷名称。
