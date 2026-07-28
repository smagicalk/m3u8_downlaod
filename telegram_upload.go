package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

const telegramUploadLogStepPercent = 5

type telegramUploadProgress struct {
	TotalBytes    int64
	UploadedBytes int64
	ChatID        int64
	ChatIndex     int
	ChatTotal     int
	StartPart     int
	EndPart       int
	TotalParts    int
	NextLogAt     int
}

func (s *telegramService) onTaskCompleted(current *task) {
	if s.currentSettings().AutoUpload {
		s.queueUpload(current.ID)
	}
}

func (s *telegramService) queueUpload(identifier string) error {
	settings := s.currentSettings()
	if !telegramConfigured(settings) {
		return errors.New("Telegram Bot 尚未完成配置")
	}
	current := s.manager.snapshot(identifier)
	if current == nil || current.Status != statusCompleted {
		return errors.New("仅已完成的任务可以上传到 Telegram")
	}
	if _, err := os.Stat(current.OutputPath); err != nil {
		return errors.New("待上传的视频文件不存在")
	}
	s.mu.Lock()
	if s.uploading == nil {
		s.uploading = make(map[string]struct{})
	}
	if s.uploadProgress == nil {
		s.uploadProgress = make(map[string]*telegramUploadProgress)
	}
	if _, exists := s.uploading[identifier]; exists {
		s.mu.Unlock()
		return errors.New("该任务正在上传到 Telegram")
	}
	s.uploading[identifier] = struct{}{}
	s.uploadProgress[identifier] = &telegramUploadProgress{NextLogAt: telegramUploadLogStepPercent}
	s.mu.Unlock()
	s.manager.addLog(identifier, "info", "Telegram 上传任务已加入队列")
	go s.upload(identifier, settings)
	return nil
}

func (s *telegramService) upload(identifier string, settings telegramSettings) {
	defer func() {
		s.mu.Lock()
		delete(s.uploading, identifier)
		delete(s.uploadProgress, identifier)
		s.mu.Unlock()
	}()
	current := s.manager.snapshot(identifier)
	if current == nil {
		return
	}
	parts, cleanup, err := splitTelegramVideo(current.OutputPath, int64(settings.SplitSizeMB)*1_000_000, current.DurationSec)
	if err != nil {
		s.manager.addLog(identifier, "error", "Telegram 视频切分失败: "+err.Error())
		return
	}
	defer cleanup()
	partBytes, err := telegramMediaBytes(parts)
	if err != nil {
		s.manager.addLog(identifier, "error", "读取 Telegram 待上传文件失败: "+err.Error())
		return
	}
	s.configureUploadProgress(identifier, partBytes*int64(len(settings.ChatIDs)), len(settings.ChatIDs), len(parts))
	s.manager.addLog(identifier, "info", fmt.Sprintf("开始上传到 Telegram，共 %d 个文件段，待传输 %s", len(parts), telegramFormatBytes(partBytes*int64(len(settings.ChatIDs)))))
	ctx := context.Background()
	for chatIndex, chatID := range settings.ChatIDs {
		if len(parts) == 1 {
			s.setUploadStage(identifier, chatID, chatIndex+1, len(settings.ChatIDs), 1, 1, len(parts))
			if err := s.sendFile(ctx, settings, "sendVideo", chatID, "video", parts[0], current.OutputName, func(delta int64) { s.recordUploadProgress(identifier, delta) }); err != nil {
				s.manager.addLog(identifier, "error", fmt.Sprintf("Telegram 上传失败（Chat %d）: %v", chatID, err))
				return
			}
			s.manager.addLog(identifier, "info", fmt.Sprintf("已上传 Telegram 视频到 Chat %d", chatID))
			continue
		}
		for start := 0; start < len(parts); {
			end := telegramMediaGroupEnd(start, len(parts))
			s.setUploadStage(identifier, chatID, chatIndex+1, len(settings.ChatIDs), start+1, end, len(parts))
			if err := s.sendMediaGroup(ctx, settings, chatID, parts[start:end], current.OutputName, start+1, len(parts), func(delta int64) { s.recordUploadProgress(identifier, delta) }); err != nil {
				s.manager.addLog(identifier, "error", fmt.Sprintf("Telegram 视频相册上传失败（Chat %d，第 %d-%d 段）: %v", chatID, start+1, end, err))
				return
			}
			s.manager.addLog(identifier, "info", fmt.Sprintf("已上传 Telegram 视频相册文件段 %d-%d/%d 到 Chat %d", start+1, end, len(parts), chatID))
			start = end
		}
	}
	s.manager.addLog(identifier, "info", "Telegram 视频上传完成")
}
