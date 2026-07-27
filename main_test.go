package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSourceURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "HTTP playlist", url: "https://example.com/video/index.m3u8", want: true},
		{name: "playlist with query", url: "https://example.com/video.m3u8?token=abc", want: true},
		{name: "unsupported scheme", url: "file:///video.m3u8", want: false},
		{name: "not a playlist", url: "https://example.com/video.mp4", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSourceURL(test.url)
			if (err == nil) != test.want {
				t.Fatalf("validateSourceURL(%q) error = %v, want valid %t", test.url, err, test.want)
			}
		})
	}
}

func TestValidateReferer(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{value: "", valid: true},
		{value: "https://example.com/watch", valid: true},
		{value: "file:///local/page", valid: false},
	} {
		err := validateReferer(test.value)
		if (err == nil) != test.valid {
			t.Fatalf("validateReferer(%q) error = %v, want valid %t", test.value, err, test.valid)
		}
	}
}

func TestValidateCookie(t *testing.T) {
	if err := validateCookie("session=abc; locale=zh-CN"); err != nil {
		t.Fatalf("valid cookie returned error: %v", err)
	}
	if err := validateCookie("session=abc\r\nX-Injected: value"); err == nil {
		t.Fatal("cookie with a line break must be rejected")
	}
}

func TestNormalizeUserAgent(t *testing.T) {
	if got := normalizeUserAgent(""); got != browserUserAgent {
		t.Fatalf("empty user agent = %q, want default", got)
	}
	if got := normalizeUserAgent("Custom Agent"); got != "Custom Agent" {
		t.Fatalf("custom user agent = %q", got)
	}
	if err := validateUserAgent("Agent\r\nInjected: value"); err == nil {
		t.Fatal("user agent with a line break must be rejected")
	}
}

func TestModeAndPlaylistDuration(t *testing.T) {
	if normalizeMode("") != modeStream || normalizeMode(modeDownloadFirst) != modeDownloadFirst {
		t.Fatal("download mode normalization failed")
	}
	if err := validateMode("unsupported"); err == nil {
		t.Fatal("unsupported download mode must be rejected")
	}
	playlist := "#EXTM3U\n#EXTINF:4.5,\npart-1.ts\n#EXTINF:2,\npart-2.ts\n"
	if got := playlistDuration(playlist); got != 6.5 {
		t.Fatalf("playlistDuration() = %f, want 6.5", got)
	}
}

func TestNormalizeConcurrentDownloads(t *testing.T) {
	if !normalizeConcurrentDownloads(nil) {
		t.Fatal("missing concurrent download option must default to enabled")
	}
	disabled := false
	if normalizeConcurrentDownloads(&disabled) {
		t.Fatal("explicitly disabled concurrent download option must remain disabled")
	}
}

func TestConcurrentDownloadsDefaultsToEnabledForAPIPayload(t *testing.T) {
	var payload createRequest
	if err := json.Unmarshal([]byte(`{"sourceUrl":"https://example.com/video.m3u8"}`), &payload); err != nil {
		t.Fatal(err)
	}
	if !normalizeConcurrentDownloads(payload.ConcurrentDownloads) {
		t.Fatal("API payload without concurrentDownloads must default to enabled")
	}
}

func TestResolveDirectory(t *testing.T) {
	resolved, err := resolveDirectory("", t.TempDir())
	if err != nil || !filepath.IsAbs(resolved) {
		t.Fatalf("resolveDirectory() = %q, %v", resolved, err)
	}
}

func TestRequestHeaders(t *testing.T) {
	got := requestHeaders("https://example.com/watch", "session=abc")
	want := "Origin: https://example.com\r\nAccept: */*\r\nAccept-Language: zh-CN,zh;q=0.9\r\nSec-Fetch-Dest: empty\r\nSec-Fetch-Mode: cors\r\nSec-Fetch-Site: cross-site\r\nCookie: session=abc\r\n"
	if got != want {
		t.Fatalf("requestHeaders() = %q, want %q", got, want)
	}
}

func TestRewritePlaylist(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\nvideo-001.ts\nvideo-002.jpeg\n"
	got := rewritePlaylist(playlist, "https://cdn.example.com/path/index.m3u8", "task-1")
	for _, expected := range []string{
		"resource/init.mp4?",
		"resource/video-001.ts?",
		"resource/video-002.ts?",
		"resource=",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("rewritten playlist missing %q: %s", expected, got)
		}
	}
}

func TestProxyURLEncoding(t *testing.T) {
	const rawURL = "https://cdn.example.com/video0.jpeg"
	decoded, err := decodeProxyURL(encodeProxyURL(rawURL))
	if err != nil || decoded != rawURL {
		t.Fatalf("proxy URL round trip = %q, %v", decoded, err)
	}
}

func TestProxyResourceName(t *testing.T) {
	if got := proxyResourceName("/video/video0.jpeg"); got != "video0.ts" {
		t.Fatalf("proxyResourceName() = %q, want video0.ts", got)
	}
}

func TestFFmpegExecutableUsesEnvironmentOverride(t *testing.T) {
	ffmpegPathOverride = ""
	t.Cleanup(func() { ffmpegPathOverride = "" })
	t.Setenv("FFMPEG_PATH", "H:/video/ffmpeg.exe")
	if got := ffmpegExecutable(); got != "H:/video/ffmpeg.exe" {
		t.Fatalf("ffmpegExecutable() = %q", got)
	}
}

func TestFFmpegExecutableUsesFlagOverride(t *testing.T) {
	ffmpegPathOverride = "H:/video/ffmpeg.exe"
	t.Cleanup(func() { ffmpegPathOverride = "" })
	if got := ffmpegExecutable(); got != "H:/video/ffmpeg.exe" {
		t.Fatalf("ffmpegExecutable() = %q", got)
	}
}

func TestNormalizeOutputName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "demo", want: "demo.mp4"},
		{input: "demo.mkv", want: "demo.mp4"},
		{input: "a/b:c", want: "abc.mp4"},
		{input: "", want: "video.mp4"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := normalizeOutputName(test.input)
			if err != nil || got != test.want {
				t.Fatalf("normalizeOutputName(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
}

func TestCancelledTaskCannotRestart(t *testing.T) {
	manager := newTaskManager(t.TempDir())
	manager.tasks["test"] = &task{ID: "test", Status: statusCancelled}

	ctx, cancel := context.WithCancel(context.Background())
	manager.mu.Lock()
	manager.cancels["test"] = cancel
	manager.mu.Unlock()

	manager.run("test")

	result := manager.snapshot("test")
	if result.Status != statusCancelled {
		t.Fatalf("cancelled task status = %q, want %q", result.Status, statusCancelled)
	}
	if _, exists := manager.cancels["test"]; exists {
		t.Fatal("cancel function should be removed after an aborted start")
	}
	if ctx.Err() != nil {
		t.Fatal("the existing context must not be cancelled by a stale runner")
	}
}

func TestTaskLogsAreBoundedAndCopied(t *testing.T) {
	manager := newTaskManager(t.TempDir())
	manager.tasks["test"] = &task{ID: "test"}
	for index := 0; index < maxTaskLogEntries+2; index++ {
		manager.addLog("test", "info", "log entry")
	}

	result := manager.snapshot("test")
	if len(result.Logs) != maxTaskLogEntries {
		t.Fatalf("log count = %d, want %d", len(result.Logs), maxTaskLogEntries)
	}
	result.Logs[0].Message = "changed outside manager"
	if manager.snapshot("test").Logs[0].Message == "changed outside manager" {
		t.Fatal("task log snapshot must not share the manager slice")
	}
}

func TestIsHTTPSource(t *testing.T) {
	if !isHTTPSource("http://127.0.0.1:8080/playlist.m3u8") || !isHTTPSource("https://example.com/video.m3u8") {
		t.Fatal("HTTP sources must enable HLS connection options")
	}
	if isHTTPSource("C:/cache/video.ts") {
		t.Fatal("local files must not receive HLS connection options")
	}
}

func TestHLSInputOptions(t *testing.T) {
	options := strings.Join(hlsInputOptions("http://127.0.0.1:8080/playlist.m3u8", true), " ")
	for _, expected := range []string{"-http_persistent 1", "-http_multiple 1", "-seg_max_retry 3"} {
		if !strings.Contains(options, expected) {
			t.Fatalf("HLS options missing %q: %s", expected, options)
		}
	}
	if got := hlsInputOptions("C:/cache/video.ts", true); got != nil {
		t.Fatalf("local input options = %v, want nil", got)
	}
}
