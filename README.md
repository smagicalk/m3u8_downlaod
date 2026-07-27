# M3U8 下载器

一个在本机运行的 Go Web 应用，用于下载 HLS/M3U8 媒体并合并为 MP4。

## 功能

- 支持自定义 Referer、Cookie 和 User-Agent。
- 通过本地 HLS 代理转发播放清单和分片请求，兼容需要浏览器请求头的站点。
- 支持“边下边合并”和“先下载后合并”两种处理方式。
- 任务列表实时显示下载、合并、暂停、继续和取消状态。
- 保存目录与缓存目录既可手动输入，也可通过 Windows 系统目录选择器选择。
- 可选在合并成功后删除临时缓存；取消或失败时会保留缓存以便检查。

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
