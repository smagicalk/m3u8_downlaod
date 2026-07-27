package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDownloadHLSMirrorsMasterPlaylistAndReusesCache(t *testing.T) {
	var requests struct {
		sync.Mutex
		resources int
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/master.m3u8":
			_, _ = writer.Write([]byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=100,AUDIO=\"audio\"\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200,AUDIO=\"audio\"\nvideo.m3u8\n"))
		case "/low.m3u8":
			_, _ = writer.Write([]byte("#EXTM3U\n#EXTINF:1,\nlow.ts\n#EXT-X-ENDLIST\n"))
		case "/video.m3u8":
			_, _ = writer.Write([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"4@0\"\n#EXTINF:2,\nvideo.ts\n#EXTINF:1,\n#EXT-X-BYTERANGE:4@0\nrange.ts\n#EXTINF:1,\n#EXT-X-BYTERANGE:4@4\nrange.ts\n#EXT-X-ENDLIST\n"))
		case "/audio.m3u8":
			_, _ = writer.Write([]byte("#EXTM3U\n#EXTINF:2,\naudio.m4a\n#EXT-X-ENDLIST\n"))
		case "/range.ts":
			requests.Lock()
			requests.resources++
			requests.Unlock()
			if got := request.Header.Get("Range"); got == "bytes=0-3" {
				writer.WriteHeader(http.StatusPartialContent)
				_, _ = writer.Write([]byte("0123"))
				return
			}
			if got := request.Header.Get("Range"); got == "bytes=4-7" {
				writer.WriteHeader(http.StatusPartialContent)
				_, _ = writer.Write([]byte("4567"))
				return
			}
			http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		case "/init.mp4":
			requests.Lock()
			requests.resources++
			requests.Unlock()
			if request.Header.Get("Range") != "bytes=0-3" {
				http.Error(writer, "invalid init range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			writer.WriteHeader(http.StatusPartialContent)
			_, _ = writer.Write([]byte("init"))
		default:
			requests.Lock()
			requests.resources++
			requests.Unlock()
			_, _ = writer.Write([]byte("fixture-data"))
		}
	}))
	defer server.Close()

	cacheDir := t.TempDir()
	config := hlsDownloadConfig{SourceURL: server.URL + "/master.m3u8", CacheDir: cacheDir, CacheKey: cacheKey(server.URL+"/master.m3u8", "fixture.mp4"), WorkerCount: 4}
	var latestProgress struct{ completed, total int }
	result, err := downloadHLS(t.Context(), config, hlsDownloadCallbacks{OnProgress: func(completed, total int, _ int64, _, _ float64) {
		latestProgress.completed, latestProgress.total = completed, total
	}})
	if err != nil {
		t.Fatalf("downloadHLS() error = %v", err)
	}
	if result.DurationSec != 4 {
		t.Fatalf("duration = %f, want 4", result.DurationSec)
	}
	if latestProgress.completed != 4 || latestProgress.total != 4 {
		t.Fatalf("segment progress = %d/%d, want 4/4", latestProgress.completed, latestProgress.total)
	}
	master, err := os.ReadFile(result.PlaylistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(master), "video.m3u8") || !strings.Contains(string(master), "audio.m3u8") || strings.Contains(string(master), "low.m3u8") {
		t.Fatalf("unexpected local master playlist: %s", master)
	}
	video, err := os.ReadFile(filepath.Join(result.CachePath, "video.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(video), server.URL) || !strings.Contains(string(video), "resources/") || !strings.Contains(string(video), "init/") || strings.Contains(string(video), "BYTERANGE") {
		t.Fatalf("video playlist was not localized: %s", video)
	}

	requests.Lock()
	firstRequestCount := requests.resources
	requests.Unlock()
	if _, err := downloadHLS(t.Context(), config, hlsDownloadCallbacks{}); err != nil {
		t.Fatalf("cache reuse download error = %v", err)
	}
	requests.Lock()
	defer requests.Unlock()
	if requests.resources != firstRequestCount {
		t.Fatalf("cached resources were downloaded again: before=%d after=%d", firstRequestCount, requests.resources)
	}
}

func TestPlanMediaPlaylistRejectsUnsupportedHLS(t *testing.T) {
	planner := &hlsResourcePlanner{root: t.TempDir(), resources: make(map[string]*hlsResource)}
	_, _, err := planMediaPlaylist("https://example.com/video.m3u8", "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key\"\n#EXT-X-ENDLIST\n", planner, true)
	if err == nil || !strings.Contains(err.Error(), "SAMPLE-AES") {
		t.Fatalf("unsupported HLS error = %v", err)
	}
}
