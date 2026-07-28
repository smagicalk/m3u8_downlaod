package main

import (
	"regexp"
	"strings"
	"sync"
)

const (
	defaultAddress  = "127.0.0.1:8080"
	downloadDir     = "downloads"
	defaultCacheDir = "cache"
	dataDir         = "data"

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

var (
	ffmpegPathOverride string
	ffmpegPathMu       sync.RWMutex
	playlistURI        = regexp.MustCompile(`URI="([^"]+)"`)
)

func configuredFFmpegPath() string {
	ffmpegPathMu.RLock()
	defer ffmpegPathMu.RUnlock()
	return strings.TrimSpace(ffmpegPathOverride)
}

func setConfiguredFFmpegPath(path string) {
	ffmpegPathMu.Lock()
	ffmpegPathOverride = strings.TrimSpace(path)
	ffmpegPathMu.Unlock()
}
