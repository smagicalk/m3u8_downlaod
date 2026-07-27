package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func validateSourceURL(raw string) error {
	parsed, err := parseHTTPURL(raw)
	if err != nil {
		return errors.New("请输入有效的 http 或 https m3u8 地址")
	}
	if !strings.Contains(strings.ToLower(parsed.Path), ".m3u8") {
		return errors.New("地址路径必须包含 .m3u8")
	}
	return nil
}

func validateReferer(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if _, err := parseHTTPURL(raw); err != nil {
		return errors.New("Referer 必须是有效的 http 或 https 地址")
	}
	return nil
}

func validateCookie(raw string) error {
	if strings.ContainsAny(raw, "\r\n") {
		return errors.New("Cookie 不能包含换行符")
	}
	return nil
}

func validateUserAgent(raw string) error {
	if strings.ContainsAny(raw, "\r\n") {
		return errors.New("User-Agent 不能包含换行符")
	}
	return nil
}

func validateMode(raw string) error {
	mode := normalizeMode(raw)
	if mode != modeStream && mode != modeDownloadFirst {
		return errors.New("下载模式无效")
	}
	return nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("invalid HTTP URL")
	}
	return parsed, nil
}

func normalizeUserAgent(raw string) string {
	if value := strings.TrimSpace(raw); value != "" {
		return value
	}
	return browserUserAgent
}

func normalizeMode(raw string) string {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		return modeStream
	}
	return mode
}

func normalizeConcurrentDownloads(raw *bool) bool {
	return raw == nil || *raw
}

func resolveDirectory(raw, fallback string) (string, error) {
	directory := strings.TrimSpace(raw)
	if directory == "" {
		directory = fallback
	}
	resolved, err := filepath.Abs(directory)
	if err != nil {
		return "", errors.New("目录路径无效")
	}
	return resolved, nil
}

func normalizeOutputName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "video"
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.Map(func(character rune) rune {
		switch character {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return -1
		default:
			return character
		}
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" || len(name) > 120 {
		return "", errors.New("输出文件名无效")
	}
	return name + ".mp4", nil
}

func nextAvailableName(directory, name string) string {
	path := filepath.Join(directory, name)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return name
	}
	extension := filepath.Ext(name)
	base := strings.TrimSuffix(name, extension)
	for index := 2; ; index++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, index, extension)
		if _, err := os.Stat(filepath.Join(directory, candidate)); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func newTaskID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("生成任务 ID 失败: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
