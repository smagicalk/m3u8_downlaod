package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var hlsByteRangeAttribute = regexp.MustCompile(`,?BYTERANGE=(?:"[^"]*"|[^,]*)`)

const (
	hlsPlaylistLimit = 16 * 1024 * 1024
	hlsRetryCount    = 3
)

type hlsDownloadConfig struct {
	SourceURL   string
	Referer     string
	Cookie      string
	UserAgent   string
	CacheDir    string
	CacheKey    string
	WorkerCount int
}

type hlsDownloadCallbacks struct {
	OnLog         func(string, string)
	OnProgress    func(completedSegments, totalSegments int, downloadedBytes int64, completedSeconds, totalSeconds float64)
	WaitForResume func(context.Context) error
}

type hlsDownloadResult struct {
	PlaylistPath string
	CachePath    string
	DurationSec  float64
}

type hlsByteRange struct {
	Offset int64
	Length int64
}

type hlsResource struct {
	URL              string
	RelativePath     string
	Range            *hlsByteRange
	ExpectedBytes    int64
	Segment          bool
	DurationSec      float64
	ProgressDuration bool
}

type hlsResourcePlanner struct {
	root      string
	resources map[string]*hlsResource
}

func downloadHLS(ctx context.Context, config hlsDownloadConfig, callbacks hlsDownloadCallbacks) (*hlsDownloadResult, error) {
	if err := validateWorkerCount(config.WorkerCount); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.CacheKey) == "" {
		return nil, errors.New("任务缓存标识无效")
	}
	cachePath := filepath.Join(config.CacheDir, "hls-"+config.CacheKey)
	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		return nil, fmt.Errorf("创建任务缓存目录失败: %w", err)
	}
	logHLS(callbacks, "info", "正在获取 HLS 播放清单")
	client := &http.Client{Timeout: 60 * time.Second}
	playlist, err := fetchPlaylist(ctx, client, config, config.SourceURL)
	if err != nil {
		return nil, err
	}
	planner := &hlsResourcePlanner{root: cachePath, resources: make(map[string]*hlsResource)}
	rootPlaylistPath, durationSec, err := planHLSDownload(ctx, client, config, planner, playlist, cachePath)
	if err != nil {
		return nil, err
	}
	resources := planner.items()
	totalSegments, completedSegments, downloadedBytes, completedSeconds := summarizeResources(resources, cachePath)
	reportHLSProgress(callbacks, completedSegments, totalSegments, downloadedBytes, completedSeconds, durationSec)
	logHLS(callbacks, "info", fmt.Sprintf("已解析 %d 个分片，准备使用 %d 个 worker 下载", totalSegments, config.WorkerCount))

	pending := make([]*hlsResource, 0, len(resources))
	for _, resource := range resources {
		if !resourceIsComplete(cachePath, resource) {
			pending = append(pending, resource)
		}
	}
	if reused := len(resources) - len(pending); reused > 0 {
		logHLS(callbacks, "info", fmt.Sprintf("已复用 %d 个完整缓存资源", reused))
	}
	if err := downloadResources(ctx, client, config, cachePath, pending, totalSegments, completedSegments, downloadedBytes, completedSeconds, durationSec, callbacks); err != nil {
		return nil, err
	}
	return &hlsDownloadResult{PlaylistPath: rootPlaylistPath, CachePath: cachePath, DurationSec: durationSec}, nil
}

func validateWorkerCount(workerCount int) error {
	switch workerCount {
	case 1, 4, 8, 16:
		return nil
	default:
		return errors.New("并发下载数仅支持 1、4、8 或 16")
	}
}

func cacheKey(sourceURL, outputName string) string {
	sum := sha256.Sum256([]byte(sourceURL + "\n" + outputName))
	return hex.EncodeToString(sum[:16])
}

func fetchPlaylist(ctx context.Context, client *http.Client, config hlsDownloadConfig, sourceURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", errors.New("创建播放清单请求失败")
	}
	applyBrowserHeaders(request, config.Referer, config.Cookie, config.UserAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("获取播放清单失败")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("播放清单服务器返回 HTTP %d", response.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, hlsPlaylistLimit))
	if err != nil {
		return "", errors.New("读取播放清单失败")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(content)), "#EXTM3U") {
		return "", errors.New("远程资源不是有效的 HLS 播放清单")
	}
	return string(content), nil
}

func planHLSDownload(ctx context.Context, client *http.Client, config hlsDownloadConfig, planner *hlsResourcePlanner, playlist, cachePath string) (string, float64, error) {
	if strings.Contains(playlist, "#EXT-X-STREAM-INF") {
		return planMasterPlaylist(ctx, client, config, planner, playlist, cachePath)
	}
	localPath := filepath.Join(cachePath, "index.m3u8")
	content, durationSec, err := planMediaPlaylist(config.SourceURL, playlist, planner, true)
	if err != nil {
		return "", 0, err
	}
	if err := writeFileAtomic(localPath, []byte(content)); err != nil {
		return "", 0, err
	}
	return localPath, durationSec, nil
}

func planMasterPlaylist(ctx context.Context, client *http.Client, config hlsDownloadConfig, planner *hlsResourcePlanner, playlist, cachePath string) (string, float64, error) {
	selection, err := selectMasterVariant(config.SourceURL, playlist)
	if err != nil {
		return "", 0, err
	}
	videoPlaylist, err := fetchPlaylist(ctx, client, config, selection.videoURL)
	if err != nil {
		return "", 0, err
	}
	videoContent, videoDuration, err := planMediaPlaylist(selection.videoURL, videoPlaylist, planner, true)
	if err != nil {
		return "", 0, err
	}
	if err := writeFileAtomic(filepath.Join(cachePath, "video.m3u8"), []byte(videoContent)); err != nil {
		return "", 0, err
	}

	masterLines := []string{"#EXTM3U"}
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "#EXT-X-VERSION") {
			masterLines = append(masterLines, line)
		}
	}
	if selection.audioURL != "" {
		audioPlaylist, err := fetchPlaylist(ctx, client, config, selection.audioURL)
		if err != nil {
			return "", 0, err
		}
		audioContent, _, err := planMediaPlaylist(selection.audioURL, audioPlaylist, planner, false)
		if err != nil {
			return "", 0, err
		}
		if err := writeFileAtomic(filepath.Join(cachePath, "audio.m3u8"), []byte(audioContent)); err != nil {
			return "", 0, err
		}
		masterLines = append(masterLines, rewriteURIAttribute(selection.audioLine, "audio.m3u8"))
	}
	masterLines = append(masterLines, selection.streamInfo, "video.m3u8")
	localPath := filepath.Join(cachePath, "index.m3u8")
	if err := writeFileAtomic(localPath, []byte(strings.Join(masterLines, "\n")+"\n")); err != nil {
		return "", 0, err
	}
	return localPath, videoDuration, nil
}

type masterSelection struct {
	videoURL   string
	streamInfo string
	audioURL   string
	audioLine  string
}

func selectMasterVariant(sourceURL, playlist string) (masterSelection, error) {
	lines := strings.Split(playlist, "\n")
	bestBandwidth := int64(-1)
	var selected masterSelection
	var audioGroup string
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		attributes := parseHLSAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		for index++; index < len(lines); index++ {
			uri := strings.TrimSpace(lines[index])
			if uri == "" {
				continue
			}
			if strings.HasPrefix(uri, "#") {
				return masterSelection{}, errors.New("主播放清单缺少媒体播放清单地址")
			}
			bandwidth, _ := strconv.ParseInt(attributes["BANDWIDTH"], 10, 64)
			if bandwidth > bestBandwidth {
				resolved, err := resolveHLSURL(sourceURL, uri)
				if err != nil {
					return masterSelection{}, err
				}
				bestBandwidth = bandwidth
				selected.videoURL = resolved
				selected.streamInfo = line
				audioGroup = attributes["AUDIO"]
			}
			break
		}
	}
	if selected.videoURL == "" {
		return masterSelection{}, errors.New("主播放清单没有可下载的视频变体")
	}
	if audioGroup == "" {
		return selected, nil
	}
	var fallbackURL, fallbackLine string
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attributes := parseHLSAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
		if attributes["TYPE"] != "AUDIO" || attributes["GROUP-ID"] != audioGroup || attributes["URI"] == "" {
			continue
		}
		resolved, err := resolveHLSURL(sourceURL, attributes["URI"])
		if err != nil {
			return masterSelection{}, err
		}
		if fallbackURL == "" {
			fallbackURL, fallbackLine = resolved, line
		}
		if strings.EqualFold(attributes["DEFAULT"], "YES") {
			selected.audioURL, selected.audioLine = resolved, line
			return selected, nil
		}
	}
	selected.audioURL, selected.audioLine = fallbackURL, fallbackLine
	return selected, nil
}

func planMediaPlaylist(sourceURL, playlist string, planner *hlsResourcePlanner, progressDuration bool) (string, float64, error) {
	if err := rejectUnsupportedHLS(playlist); err != nil {
		return "", 0, err
	}
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		return "", 0, errors.New("仅支持包含 EXT-X-ENDLIST 的点播播放清单")
	}
	var output []string
	var durationSec float64
	var pendingDuration float64
	var pendingRange *hlsByteRange
	var nextRangeOffset int64
	for _, rawLine := range strings.Split(playlist, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#EXTINF:") {
			pendingDuration = parseExtinfDuration(trimmed)
			durationSec += pendingDuration
			output = append(output, line)
			continue
		}
		if strings.HasPrefix(trimmed, "#EXT-X-BYTERANGE:") {
			byteRange, err := parseByteRange(strings.TrimPrefix(trimmed, "#EXT-X-BYTERANGE:"), nextRangeOffset)
			if err != nil {
				return "", 0, err
			}
			pendingRange = &byteRange
			nextRangeOffset = byteRange.Offset + byteRange.Length
			continue
		}
		if strings.HasPrefix(trimmed, "#EXT-X-KEY:") || strings.HasPrefix(trimmed, "#EXT-X-MAP:") {
			attributes := parseHLSAttributes(strings.TrimPrefix(strings.TrimPrefix(trimmed, "#EXT-X-KEY:"), "#EXT-X-MAP:"))
			if strings.HasPrefix(trimmed, "#EXT-X-KEY:") && strings.EqualFold(attributes["METHOD"], "NONE") {
				output = append(output, line)
				continue
			}
			if strings.HasPrefix(trimmed, "#EXT-X-KEY:") && !strings.EqualFold(attributes["METHOD"], "AES-128") {
				return "", 0, fmt.Errorf("暂不支持 HLS 加密方式：%s", attributes["METHOD"])
			}
			uri, ok := hlsAttributeURI(trimmed)
			if !ok {
				return "", 0, errors.New("播放清单资源标签缺少 URI")
			}
			resolved, err := resolveHLSURL(sourceURL, uri)
			if err != nil {
				return "", 0, err
			}
			kind := "resources"
			if strings.HasPrefix(trimmed, "#EXT-X-MAP:") {
				kind = "init"
			}
			var mapRange *hlsByteRange
			if value := attributes["BYTERANGE"]; value != "" {
				byteRange, err := parseByteRange(value, 0)
				if err != nil {
					return "", 0, err
				}
				mapRange = &byteRange
			}
			localPath := planner.add(resolved, kind, mapRange, false, 0, false)
			localizedLine := rewriteURIAttribute(line, localPath)
			if mapRange != nil {
				localizedLine = hlsByteRangeAttribute.ReplaceAllString(localizedLine, "")
			}
			output = append(output, localizedLine)
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			output = append(output, line)
			continue
		}
		resolved, err := resolveHLSURL(sourceURL, trimmed)
		if err != nil {
			return "", 0, err
		}
		localPath := planner.add(resolved, "segments", pendingRange, true, pendingDuration, progressDuration)
		output = append(output, localPath)
		pendingRange = nil
		pendingDuration = 0
	}
	return strings.Join(output, "\n"), durationSec, nil
}

func rejectUnsupportedHLS(playlist string) error {
	unsupported := []string{"#EXT-X-PART", "#EXT-X-SERVER-CONTROL", "#EXT-X-PRELOAD-HINT", "SAMPLE-AES", "#EXT-X-SESSION-KEY"}
	for _, marker := range unsupported {
		if strings.Contains(playlist, marker) {
			return fmt.Errorf("暂不支持 HLS 标签或加密方式：%s", marker)
		}
	}
	return nil
}

func (planner *hlsResourcePlanner) add(resourceURL, kind string, byteRange *hlsByteRange, segment bool, durationSec float64, progressDuration bool) string {
	identity := resourceURL
	if byteRange != nil {
		identity += fmt.Sprintf("#%d:%d", byteRange.Offset, byteRange.Length)
	}
	sum := sha256.Sum256([]byte(identity))
	relativePath := path.Join(kind, hex.EncodeToString(sum[:])+resourceExtension(resourceURL, kind))
	if existing, ok := planner.resources[identity]; ok {
		return existing.RelativePath
	}
	resource := &hlsResource{URL: resourceURL, RelativePath: relativePath, Segment: segment, DurationSec: durationSec, ProgressDuration: progressDuration}
	if byteRange != nil {
		resource.Range = &hlsByteRange{Offset: byteRange.Offset, Length: byteRange.Length}
		resource.ExpectedBytes = byteRange.Length
	}
	planner.resources[identity] = resource
	return relativePath
}

func (planner *hlsResourcePlanner) items() []*hlsResource {
	items := make([]*hlsResource, 0, len(planner.resources))
	for _, resource := range planner.resources {
		items = append(items, resource)
	}
	return items
}

func resourceExtension(resourceURL, kind string) string {
	if kind == "resources" {
		return ".key"
	}
	parsed, err := url.Parse(resourceURL)
	if err == nil {
		extension := strings.ToLower(path.Ext(parsed.Path))
		switch extension {
		case ".ts", ".m2ts", ".m4s", ".mp4", ".m4a", ".aac", ".mp3", ".vtt", ".webvtt":
			return extension
		}
	}
	return ".ts"
}

func resolveHLSURL(baseURL, reference string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", errors.New("播放清单地址无效")
	}
	resolved, err := base.Parse(reference)
	if err != nil || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return "", errors.New("播放清单包含无效资源地址")
	}
	return resolved.String(), nil
}

func parseHLSAttributes(raw string) map[string]string {
	attributes := make(map[string]string)
	for len(raw) > 0 {
		raw = strings.TrimLeft(raw, " ,")
		separator := strings.IndexByte(raw, '=')
		if separator <= 0 {
			break
		}
		key := raw[:separator]
		raw = raw[separator+1:]
		value := ""
		if strings.HasPrefix(raw, "\"") {
			raw = raw[1:]
			end := strings.IndexByte(raw, '"')
			if end < 0 {
				break
			}
			value, raw = raw[:end], raw[end+1:]
		} else {
			end := strings.IndexByte(raw, ',')
			if end < 0 {
				value, raw = raw, ""
			} else {
				value, raw = raw[:end], raw[end+1:]
			}
		}
		attributes[key] = value
	}
	return attributes
}

func hlsAttributeURI(line string) (string, bool) {
	parts := playlistURI.FindStringSubmatch(line)
	if len(parts) != 2 {
		return "", false
	}
	return parts[1], true
}

func rewriteURIAttribute(line, localPath string) string {
	return playlistURI.ReplaceAllString(line, `URI="`+localPath+`"`)
}

func parseExtinfDuration(line string) float64 {
	value := strings.TrimPrefix(line, "#EXTINF:")
	value = strings.SplitN(value, ",", 2)[0]
	duration, err := strconv.ParseFloat(value, 64)
	if err != nil || duration < 0 {
		return 0
	}
	return duration
}

func parseByteRange(raw string, implicitOffset int64) (hlsByteRange, error) {
	parts := strings.SplitN(strings.TrimSpace(raw), "@", 2)
	length, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || length <= 0 {
		return hlsByteRange{}, errors.New("播放清单包含无效的字节范围")
	}
	offset := implicitOffset
	if len(parts) == 2 {
		offset, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || offset < 0 {
			return hlsByteRange{}, errors.New("播放清单包含无效的字节范围偏移")
		}
	}
	return hlsByteRange{Offset: offset, Length: length}, nil
}

func summarizeResources(resources []*hlsResource, root string) (totalSegments, completedSegments int, downloadedBytes int64, completedSeconds float64) {
	for _, resource := range resources {
		if !resource.Segment {
			continue
		}
		totalSegments++
		if !resourceIsComplete(root, resource) {
			continue
		}
		completedSegments++
		if resource.ProgressDuration {
			completedSeconds += resource.DurationSec
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(resource.RelativePath)))
		if err == nil {
			downloadedBytes += info.Size()
		}
	}
	return totalSegments, completedSegments, downloadedBytes, completedSeconds
}

func resourceIsComplete(root string, resource *hlsResource) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(resource.RelativePath)))
	if err != nil || info.Size() <= 0 {
		return false
	}
	return resource.ExpectedBytes == 0 || info.Size() == resource.ExpectedBytes
}

func downloadResources(ctx context.Context, client *http.Client, config hlsDownloadConfig, root string, resources []*hlsResource, totalSegments, completedSegments int, downloadedBytes int64, completedSeconds, totalSeconds float64, callbacks hlsDownloadCallbacks) error {
	if len(resources) == 0 {
		return nil
	}
	jobs := make(chan *hlsResource)
	errorsChannel := make(chan error, 1)
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	var state struct {
		sync.Mutex
		completedSegments int
		downloadedBytes   int64
		completedSeconds  float64
	}
	state.completedSegments = completedSegments
	state.downloadedBytes = downloadedBytes
	state.completedSeconds = completedSeconds
	var workers sync.WaitGroup
	worker := func() {
		defer workers.Done()
		for resource := range jobs {
			if callbacks.WaitForResume != nil {
				if err := callbacks.WaitForResume(workerContext); err != nil {
					return
				}
			}
			bytesWritten, err := downloadResource(workerContext, client, config, root, resource, callbacks)
			if err != nil {
				select {
				case errorsChannel <- err:
					cancel()
				default:
				}
				return
			}
			state.Lock()
			state.downloadedBytes += bytesWritten
			if resource.Segment {
				state.completedSegments++
				if resource.ProgressDuration {
					state.completedSeconds += resource.DurationSec
				}
			}
			reportHLSProgress(callbacks, state.completedSegments, totalSegments, state.downloadedBytes, state.completedSeconds, totalSeconds)
			state.Unlock()
		}
	}
	workers.Add(config.WorkerCount)
	for index := 0; index < config.WorkerCount; index++ {
		go worker()
	}
enqueue:
	for _, resource := range resources {
		select {
		case jobs <- resource:
		case <-workerContext.Done():
			break enqueue
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-errorsChannel:
		return err
	default:
		if ctx.Err() != nil {
			return errors.New("任务已取消")
		}
	}
	return nil
}

func downloadResource(ctx context.Context, client *http.Client, config hlsDownloadConfig, root string, resource *hlsResource, callbacks hlsDownloadCallbacks) (int64, error) {
	var lastError error
	for attempt := 0; attempt <= hlsRetryCount; attempt++ {
		if attempt > 0 {
			logHLS(callbacks, "warning", fmt.Sprintf("分片下载失败，正在进行第 %d 次重试", attempt))
			if err := waitRetry(ctx, time.Duration(attempt)*400*time.Millisecond); err != nil {
				return 0, errors.New("任务已取消")
			}
		}
		bytesWritten, err := downloadResourceOnce(ctx, client, config, root, resource)
		if err == nil {
			return bytesWritten, nil
		}
		lastError = err
		if ctx.Err() != nil {
			return 0, errors.New("任务已取消")
		}
	}
	return 0, fmt.Errorf("分片下载失败: %w", lastError)
}

func downloadResourceOnce(ctx context.Context, client *http.Client, config hlsDownloadConfig, root string, resource *hlsResource) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resource.URL, nil)
	if err != nil {
		return 0, errors.New("创建分片请求失败")
	}
	applyBrowserHeaders(request, config.Referer, config.Cookie, config.UserAgent)
	if resource.Range != nil {
		end := resource.Range.Offset + resource.Range.Length - 1
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", resource.Range.Offset, end))
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, errors.New("获取远程分片失败")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("远程分片服务器返回 HTTP %d", response.StatusCode)
	}
	if resource.Range != nil && response.StatusCode != http.StatusPartialContent {
		return 0, errors.New("远程服务器未响应分片字节范围请求")
	}
	path := filepath.Join(root, filepath.FromSlash(resource.RelativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("创建分片缓存目录失败: %w", err)
	}
	temporaryPath := path + ".part"
	file, err := os.Create(temporaryPath)
	if err != nil {
		return 0, fmt.Errorf("创建临时分片失败: %w", err)
	}
	bytesWritten, copyErr := io.Copy(file, response.Body)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(temporaryPath)
		return 0, errors.New("写入分片缓存失败")
	}
	if resource.ExpectedBytes > 0 && bytesWritten != resource.ExpectedBytes {
		_ = os.Remove(temporaryPath)
		return 0, errors.New("分片字节范围长度不匹配")
	}
	if bytesWritten == 0 {
		_ = os.Remove(temporaryPath)
		return 0, errors.New("远程分片为空")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return 0, fmt.Errorf("保存分片缓存失败: %w", err)
	}
	return bytesWritten, nil
}

func waitRetry(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeFileAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporaryPath := path + ".part"
	if err := os.WriteFile(temporaryPath, content, 0o644); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func logHLS(callbacks hlsDownloadCallbacks, level, message string) {
	if callbacks.OnLog != nil {
		callbacks.OnLog(level, message)
	}
}

func reportHLSProgress(callbacks hlsDownloadCallbacks, completedSegments, totalSegments int, downloadedBytes int64, completedSeconds, totalSeconds float64) {
	if callbacks.OnProgress != nil {
		callbacks.OnProgress(completedSegments, totalSegments, downloadedBytes, completedSeconds, totalSeconds)
	}
}
