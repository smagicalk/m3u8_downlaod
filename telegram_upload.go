package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

const telegramUploadLogStepPercent = 5
const telegramMultipartSafeLimitBytes int64 = 3_900_000_000

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
			group := parts[start:end]
			groupBytes, err := telegramMediaBytes(group)
			if err != nil {
				s.manager.addLog(identifier, "error", "读取 Telegram 相册文件段失败: "+err.Error())
				return
			}
			s.setUploadStage(identifier, chatID, chatIndex+1, len(settings.ChatIDs), start+1, end, len(parts))
			progress := func(delta int64) { s.recordUploadProgress(identifier, delta) }
			var uploadErr error
			if telegramMediaGroupNeedsStaging(groupBytes) {
				s.manager.addLog(identifier, "info", fmt.Sprintf("相册文件段 %d-%d 总计 %s，改用 file_id 暂存以避开 Bot API 4000 MB 请求上限", start+1, end, telegramFormatBytes(groupBytes)))
				uploadErr = s.sendMediaGroupUsingFileIDs(ctx, settings, chatID, group, current.OutputName, start+1, len(parts), progress)
			} else {
				uploadErr = s.sendMediaGroup(ctx, settings, chatID, group, current.OutputName, start+1, len(parts), progress)
			}
			if uploadErr != nil {
				s.manager.addLog(identifier, "error", fmt.Sprintf("Telegram 视频相册上传失败（Chat %d，第 %d-%d 段）: %v", chatID, start+1, end, uploadErr))
				return
			}
			s.manager.addLog(identifier, "info", fmt.Sprintf("已上传 Telegram 视频相册文件段 %d-%d/%d 到 Chat %d", start+1, end, len(parts), chatID))
			start = end
		}
	}
	s.manager.addLog(identifier, "info", "Telegram 视频上传完成")
}

func telegramMediaGroupNeedsStaging(totalBytes int64) bool {
	return totalBytes > telegramMultipartSafeLimitBytes
}

func telegramMediaBytes(paths []string) (int64, error) {
	var total int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func (s *telegramService) configureUploadProgress(identifier string, totalBytes int64, chatTotal, totalParts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress := s.uploadProgress[identifier]
	if progress == nil {
		return
	}
	progress.TotalBytes = totalBytes
	progress.ChatTotal = chatTotal
	progress.TotalParts = totalParts
}

func (s *telegramService) setUploadStage(identifier string, chatID int64, chatIndex, chatTotal, startPart, endPart, totalParts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress := s.uploadProgress[identifier]
	if progress == nil {
		return
	}
	progress.ChatID = chatID
	progress.ChatIndex = chatIndex
	progress.ChatTotal = chatTotal
	progress.StartPart = startPart
	progress.EndPart = endPart
	progress.TotalParts = totalParts
}

func (s *telegramService) recordUploadProgress(identifier string, delta int64) {
	if delta <= 0 {
		return
	}
	var message string
	s.mu.Lock()
	progress := s.uploadProgress[identifier]
	if progress != nil {
		progress.UploadedBytes += delta
		if progress.TotalBytes > 0 && progress.UploadedBytes > progress.TotalBytes {
			progress.UploadedBytes = progress.TotalBytes
		}
		percent := telegramUploadPercent(progress)
		if percent >= progress.NextLogAt || (progress.TotalBytes > 0 && progress.UploadedBytes == progress.TotalBytes) {
			for progress.NextLogAt <= percent {
				progress.NextLogAt += telegramUploadLogStepPercent
			}
			message = "Telegram 上传进度：" + telegramUploadProgressText(progress)
		}
	}
	s.mu.Unlock()
	if message != "" {
		s.manager.addLog(identifier, "info", message)
	}
}

func (s *telegramService) uploadProgressSnapshot(identifier string) *telegramUploadProgress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	progress := s.uploadProgress[identifier]
	if progress == nil {
		return nil
	}
	copy := *progress
	return &copy
}

func (s *telegramService) isUploading(identifier string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, uploading := s.uploading[identifier]
	return uploading
}

func telegramUploadPercent(progress *telegramUploadProgress) int {
	if progress == nil || progress.TotalBytes <= 0 {
		return 0
	}
	return int(progress.UploadedBytes * 100 / progress.TotalBytes)
}

func telegramUploadProgressText(progress *telegramUploadProgress) string {
	if progress == nil || progress.TotalBytes <= 0 {
		return "准备上传"
	}
	return fmt.Sprintf("%d%%（%s / %s）\n目标：Chat %d（%d/%d），文件段 %d-%d/%d", telegramUploadPercent(progress), telegramFormatBytes(progress.UploadedBytes), telegramFormatBytes(progress.TotalBytes), progress.ChatID, progress.ChatIndex, progress.ChatTotal, progress.StartPart, progress.EndPart, progress.TotalParts)
}

func telegramFormatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes)
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == "TB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

func telegramMediaGroupEnd(start, total int) int {
	end := min(start+telegramMediaGroupMaxItems, total)
	if total-end == 1 {
		end--
	}
	return end
}
