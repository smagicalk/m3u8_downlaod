package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const telegramSegmentTargetPercent int64 = 90
const telegramSegmentRetryCount = 4

func splitTelegramVideo(path string, maxBytes int64, durationSec float64) ([]string, func(), error) {
	if maxBytes <= 0 {
		return nil, nil, errors.New("切分大小无效")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Size() <= maxBytes {
		return []string{path}, func() {}, nil
	}
	if durationSec <= 0 {
		durationSec, err = probeTelegramMediaDuration(context.Background(), path)
		if err != nil {
			return nil, nil, err
		}
	}
	targetBytes := maxBytes * telegramSegmentTargetPercent / 100
	segmentDuration := telegramSegmentDuration(info.Size(), durationSec, targetBytes)
	directory, err := os.MkdirTemp("", "m3u8-telegram-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	for attempt := 0; attempt < telegramSegmentRetryCount; attempt++ {
		attemptDirectory := filepath.Join(directory, fmt.Sprintf("attempt-%d", attempt+1))
		if err := os.MkdirAll(attemptDirectory, 0o755); err != nil {
			cleanup()
			return nil, nil, err
		}
		pattern := filepath.Join(attemptDirectory, "part-%03d.mp4")
		if err := runTelegramSegmentFFmpeg(context.Background(), path, pattern, segmentDuration); err != nil {
			cleanup()
			return nil, nil, err
		}
		parts, largest, err := telegramSegmentParts(attemptDirectory)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if len(parts) >= 2 && largest <= maxBytes {
			return parts, cleanup, nil
		}
		segmentDuration = telegramSegmentDuration(largest, segmentDuration, targetBytes)
	}
	cleanup()
	return nil, nil, errors.New("FFmpeg 切分后仍有文件段超过 Telegram 大小限制")
}

func telegramSegmentDuration(totalBytes int64, durationSec float64, targetBytes int64) float64 {
	if totalBytes <= 0 || durationSec <= 0 || targetBytes <= 0 {
		return 1
	}
	seconds := durationSec * float64(targetBytes) / float64(totalBytes)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func telegramSegmentArguments(inputPath, outputPattern string, durationSec float64) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", inputPath,
		"-map", "0:v?", "-map", "0:a?",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%.3f", durationSec),
		"-reset_timestamps", "1",
		"-avoid_negative_ts", "make_zero",
		"-segment_format", "mp4",
		"-segment_format_options", "movflags=+faststart",
		outputPattern,
	}
}

func runTelegramSegmentFFmpeg(ctx context.Context, inputPath, outputPattern string, durationSec float64) error {
	executable := ffmpegExecutable()
	if _, err := exec.LookPath(executable); err != nil {
		return errors.New("未找到 ffmpeg，无法切分 Telegram 视频")
	}
	output, err := exec.CommandContext(ctx, executable, telegramSegmentArguments(inputPath, outputPattern, durationSec)...).CombinedOutput()
	if err != nil {
		message := lastMeaningfulLine(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("FFmpeg 切分失败: %s", message)
	}
	return nil
}

func telegramSegmentParts(directory string) ([]string, int64, error) {
	parts, err := filepath.Glob(filepath.Join(directory, "part-*.mp4"))
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return nil, 0, errors.New("FFmpeg 未生成可上传的视频文件段")
	}
	var largest int64
	for _, part := range parts {
		info, err := os.Stat(part)
		if err != nil {
			return nil, 0, err
		}
		if info.Size() == 0 {
			return nil, 0, errors.New("FFmpeg 生成了空视频文件段")
		}
		if info.Size() > largest {
			largest = info.Size()
		}
	}
	return parts, largest, nil
}

func probeTelegramMediaDuration(ctx context.Context, path string) (float64, error) {
	executable := telegramFFprobeExecutable()
	if _, err := exec.LookPath(executable); err != nil {
		return 0, errors.New("未找到 ffprobe，无法读取视频时长")
	}
	output, err := exec.CommandContext(ctx, executable, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, errors.New("ffprobe 无法读取视频时长")
	}
	durationSec, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || durationSec <= 0 {
		return 0, errors.New("ffprobe 返回无效的视频时长")
	}
	return durationSec, nil
}

func telegramFFprobeExecutable() string {
	ffmpegPath := ffmpegExecutable()
	extension := filepath.Ext(ffmpegPath)
	if strings.EqualFold(filepath.Base(ffmpegPath), "ffmpeg"+extension) {
		candidate := filepath.Join(filepath.Dir(ffmpegPath), "ffprobe"+extension)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "ffprobe"
}
