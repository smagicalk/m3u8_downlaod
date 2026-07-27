# M3U8 下载器

一个在本机运行的 Go Web 应用，用于下载 HLS/M3U8 媒体并合并为 MP4。

## 功能

- 支持自定义 Referer、Cookie 和 User-Agent。
- Go 负责解析播放清单并并发下载分片；FFmpeg 只读取本地缓存播放清单并合并 MP4。
- 支持 `1`、`4`、`8`、`16` 个分片 worker，默认 `8`；支持主/媒体清单、TS、fMP4、AES-128、初始化分片和字节范围点播。
- 任务列表实时显示下载、合并、暂停、继续和取消状态。
- 保存目录与缓存目录既可手动输入，也可通过 Windows 系统目录选择器选择。
- 可选在合并成功后删除临时缓存；取消或失败时会保留缓存。重新提交相同来源和输出名时会自动复用已完成分片。

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

请仅下载你拥有访问和保存权限的媒体内容。
