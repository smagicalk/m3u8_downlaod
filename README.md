# M3U8 下载器

一个在本机运行的 Go Web 应用，用于下载 HLS/M3U8 媒体并合并为 MP4。

## 功能

- 支持自定义 Referer、Cookie 和 User-Agent。
- Go 负责解析播放清单并并发下载分片；FFmpeg 只读取本地缓存播放清单并合并 MP4。
- 支持 `1`、`4`、`8`、`16` 个分片 worker，默认 `8`；支持主/媒体清单、TS、fMP4、AES-128、初始化分片和字节范围点播。
- 任务列表实时显示下载、合并、暂停、继续和取消状态。
- 使用 SQLite 持久化任务、运行日志和默认下载设置；服务重启后仍可查看历史任务。
- 保存目录与缓存目录既可手动输入，也可通过 Windows 系统目录选择器选择。
- 可选在合并成功后删除临时缓存；取消或失败时会保留缓存。重新提交相同来源和输出名时会自动复用已完成分片。
- 内置本地管理员登录、退出和密码修改；密码仅以 bcrypt 散列形式保存在 SQLite。

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

请仅下载你拥有访问和保存权限的媒体内容。
