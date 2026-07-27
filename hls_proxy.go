package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

func (m *taskManager) servePlaylistProxy(writer http.ResponseWriter, request *http.Request) {
	m.serveProxyResource(writer, request, "")
}

func (m *taskManager) serveResourceProxy(writer http.ResponseWriter, request *http.Request) {
	identifier := request.PathValue("id")
	resourceURL, err := decodeProxyURL(request.URL.Query().Get("resource"))
	if err != nil {
		m.addLog(identifier, "warning", "代理资源地址无效")
		writeError(writer, http.StatusBadRequest, "代理资源地址无效")
		return
	}
	m.serveProxyResource(writer, request, resourceURL)
}

func (m *taskManager) serveProxyResource(writer http.ResponseWriter, request *http.Request, resourceURL string) {
	identifier := request.PathValue("id")
	m.mu.RLock()
	current := m.tasks[identifier]
	if current == nil {
		m.mu.RUnlock()
		writeError(writer, http.StatusNotFound, "任务不存在")
		return
	}
	sourceURL, referer, cookie, userAgent := current.SourceURL, current.Referer, current.Cookie, current.UserAgent
	m.mu.RUnlock()
	if resourceURL == "" {
		resourceURL = sourceURL
	}
	if _, err := parseHTTPURL(resourceURL); err != nil {
		m.addLog(identifier, "warning", "代理资源地址无效")
		writeError(writer, http.StatusBadRequest, "代理资源地址无效")
		return
	}

	remoteRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, resourceURL, nil)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "创建代理请求失败")
		return
	}
	applyBrowserHeaders(remoteRequest, referer, cookie, userAgent)
	response, err := m.httpClient.Do(remoteRequest)
	if err != nil {
		m.addLog(identifier, "error", "HLS 代理无法连接远程资源")
		writeError(writer, http.StatusBadGateway, "获取远程资源失败")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		m.addLog(identifier, "error", fmt.Sprintf("远程服务器返回 HTTP %d", response.StatusCode))
		writeError(writer, http.StatusBadGateway, fmt.Sprintf("远程服务器返回 HTTP %d", response.StatusCode))
		return
	}

	if isPlaylist(resourceURL, response.Header.Get("Content-Type")) {
		content, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
		if err != nil {
			m.addLog(identifier, "error", "读取播放清单失败")
			writeError(writer, http.StatusBadGateway, "读取播放清单失败")
			return
		}
		m.setDuration(identifier, playlistDuration(string(content)))
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
		_, _ = io.WriteString(writer, rewritePlaylist(string(content), resourceURL, identifier))
		return
	}
	copyProxyHeaders(writer.Header(), response.Header)
	if strings.HasSuffix(strings.ToLower(request.PathValue("name")), ".ts") {
		writer.Header().Set("Content-Type", "video/MP2T")
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func (m *taskManager) setDuration(identifier string, duration float64) {
	if duration <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil && duration > current.DurationSec {
		current.DurationSec = duration
	}
}

func playlistDuration(content string) float64 {
	var total float64
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		value := strings.TrimPrefix(line, "#EXTINF:")
		value = strings.SplitN(value, ",", 2)[0]
		seconds, err := strconv.ParseFloat(value, 64)
		if err == nil && seconds > 0 {
			total += seconds
		}
	}
	return total
}

func applyBrowserHeaders(request *http.Request, referer, cookie, userAgent string) {
	request.Header.Set("User-Agent", normalizeUserAgent(userAgent))
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if referer != "" {
		request.Header.Set("Referer", referer)
		request.Header.Set("Sec-Fetch-Dest", "empty")
		request.Header.Set("Sec-Fetch-Mode", "cors")
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		if parsed, err := parseHTTPURL(referer); err == nil {
			request.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
		}
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
}

// requestHeaders 保留为可复用的 FFmpeg 风格请求头表示，供测试和后续直连模式使用。
func requestHeaders(referer, cookie string) string {
	headers := make([]string, 0, 7)
	if referer != "" {
		if parsed, err := parseHTTPURL(referer); err == nil {
			headers = append(headers,
				"Origin: "+parsed.Scheme+"://"+parsed.Host,
				"Accept: */*",
				"Accept-Language: zh-CN,zh;q=0.9",
				"Sec-Fetch-Dest: empty",
				"Sec-Fetch-Mode: cors",
				"Sec-Fetch-Site: cross-site",
			)
		}
	}
	if cookie != "" {
		headers = append(headers, "Cookie: "+cookie)
	}
	if len(headers) == 0 {
		return ""
	}
	return strings.Join(headers, "\r\n") + "\r\n"
}

func isPlaylist(resourceURL, contentType string) bool {
	parsed, err := url.Parse(resourceURL)
	if err == nil && strings.Contains(strings.ToLower(parsed.Path), ".m3u8") {
		return true
	}
	return strings.Contains(strings.ToLower(contentType), "mpegurl")
}

func rewritePlaylist(content, sourceURL, identifier string) string {
	baseURL, err := url.Parse(sourceURL)
	if err != nil {
		return content
	}
	proxyURL := func(reference string) string {
		resolved, err := baseURL.Parse(reference)
		if err != nil || resolved.Scheme != "http" && resolved.Scheme != "https" {
			return reference
		}
		name := proxyResourceName(resolved.Path)
		return "/api/proxy/" + url.PathEscape(identifier) + "/resource/" + url.PathEscape(name) + "?resource=" + encodeProxyURL(resolved.String())
	}
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			lines[index] = proxyURL(trimmed)
			continue
		}
		lines[index] = playlistURI.ReplaceAllStringFunc(line, func(match string) string {
			parts := playlistURI.FindStringSubmatch(match)
			if len(parts) != 2 {
				return match
			}
			return `URI="` + proxyURL(parts[1]) + `"`
		})
	}
	return strings.Join(lines, "\n")
}

func encodeProxyURL(rawURL string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(rawURL))
}

func decodeProxyURL(encoded string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func proxyResourceName(resourcePath string) string {
	name := path.Base(resourcePath)
	if name == "." || name == "/" || name == "" {
		return "resource.ts"
	}
	extension := strings.ToLower(path.Ext(name))
	switch extension {
	case ".ts", ".m2ts", ".m4s", ".mp4", ".m4a", ".aac", ".mp3", ".vtt", ".webvtt", ".m3u8":
		return name
	default:
		return strings.TrimSuffix(name, path.Ext(name)) + ".ts"
	}
}

func copyProxyHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := source.Get(name); value != "" {
			destination.Set(name, value)
		}
	}
}
