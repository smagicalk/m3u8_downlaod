package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

func runFFmpeg(ctx context.Context, sourceURL, outputPath, outputFormat string, onProgress func(float64), onProcess func(*os.Process)) error {
	executable := ffmpegExecutable()
	if _, err := exec.LookPath(executable); err != nil {
		return errors.New("未找到 ffmpeg，请检查 FFMPEG_PATH 或系统 PATH 后重试")
	}
	arguments := []string{
		"-hide_banner", "-nostdin", "-y",
		"-progress", "pipe:1",
		"-rw_timeout", "30000000",
	}
	arguments = append(arguments,
		"-i", sourceURL,
		"-map", "0",
		"-c", "copy",
	)
	if outputFormat == "mpegts" {
		arguments = append(arguments, "-f", "mpegts", outputPath)
	} else {
		arguments = append(arguments, "-movflags", "+faststart", outputPath)
	}
	cmd := exec.CommandContext(ctx, executable, arguments...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("读取 ffmpeg 进度失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("读取 ffmpeg 错误输出失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 ffmpeg 失败: %w", err)
	}
	onProcess(cmd.Process)

	var wait sync.WaitGroup
	var stderrText string
	wait.Add(2)
	go func() {
		defer wait.Done()
		readProgress(stdout, onProgress)
	}()
	go func() {
		defer wait.Done()
		data, _ := io.ReadAll(io.LimitReader(stderr, 16*1024))
		stderrText = string(data)
	}()
	err = cmd.Wait()
	wait.Wait()
	if ctx.Err() != nil {
		return errors.New("任务已取消")
	}
	if err != nil {
		message := lastMeaningfulLine(stderrText)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("ffmpeg 执行失败: %s", message)
	}
	return nil
}

func ffmpegExecutable() string {
	if configured := strings.TrimSpace(ffmpegPathOverride); configured != "" {
		return configured
	}
	if configured := strings.TrimSpace(os.Getenv("FFMPEG_PATH")); configured != "" {
		return configured
	}
	return "ffmpeg"
}

func readProgress(reader io.Reader, onProgress func(float64)) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		parts := strings.SplitN(strings.TrimSpace(scanner.Text()), "=", 2)
		if len(parts) != 2 || parts[0] != "out_time_us" {
			continue
		}
		var microseconds int64
		if _, err := fmt.Sscan(parts[1], &microseconds); err == nil && microseconds >= 0 {
			onProgress(float64(microseconds) / 1_000_000)
		}
	}
}

func lastMeaningfulLine(text string) string {
	lines := strings.Split(text, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line != "" {
			return line
		}
	}
	return ""
}
