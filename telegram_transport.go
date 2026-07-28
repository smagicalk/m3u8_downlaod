package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const telegramMediaGroupMaxItems = 10

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type telegramInputMedia struct {
	Type    string `json:"type"`
	Media   string `json:"media"`
	Caption string `json:"caption,omitempty"`
}

type telegramSentVideo struct {
	MessageID int `json:"message_id"`
	Video     struct {
		FileID string `json:"file_id"`
	} `json:"video"`
}

func (s *telegramService) sendMessage(ctx context.Context, settings telegramSettings, chatID int64, text string, keyboard any) error {
	return s.sendMessageResult(ctx, settings, chatID, text, keyboard, nil)
}

func (s *telegramService) sendMessageResult(ctx context.Context, settings telegramSettings, chatID int64, text string, keyboard any, result any) error {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	return s.call(ctx, settings, "sendMessage", payload, result)
}

func (s *telegramService) call(ctx context.Context, settings telegramSettings, method string, payload any, result any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(settings.APIBaseURL, "/")+"/bot"+settings.BotToken+"/"+method, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return s.sendTelegramRequest(request, result)
}

func (s *telegramService) sendFile(ctx context.Context, settings telegramSettings, method string, chatID int64, field, path, caption string, progress func(int64)) error {
	return s.sendFileResult(ctx, settings, method, chatID, field, path, caption, progress, nil)
}

func (s *telegramService) sendFileResult(ctx context.Context, settings telegramSettings, method string, chatID int64, field, path, caption string, progress func(int64), result any) error {
	source, err := os.Open(path)
	if err != nil {
		return errors.New("读取待上传文件失败")
	}
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(settings.APIBaseURL, "/")+"/bot"+settings.BotToken+"/"+method, reader)
	if err != nil {
		_ = source.Close()
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	go func() {
		defer source.Close()
		if err := writeTelegramFileForm(form, chatID, field, path, caption, source, progress); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	defer reader.Close()
	return s.sendTelegramUploadRequest(request, result)
}

func (s *telegramService) sendMediaGroup(ctx context.Context, settings telegramSettings, chatID int64, paths []string, outputName string, startIndex, totalParts int, progress func(int64)) ([]telegramMessage, error) {
	if len(paths) < 2 || len(paths) > telegramMediaGroupMaxItems {
		return nil, errors.New("Telegram 相册文件段数量必须为 2 至 10")
	}
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(settings.APIBaseURL, "/")+"/bot"+settings.BotToken+"/sendMediaGroup", reader)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	go func() {
		if err := writeTelegramMediaGroupForm(form, chatID, paths, outputName, startIndex, totalParts, progress); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	defer reader.Close()
	var sent []telegramMessage
	if err := s.sendTelegramUploadRequest(request, &sent); err != nil {
		return nil, err
	}
	return sent, nil
}

func (s *telegramService) sendMediaGroupUsingFileIDs(ctx context.Context, settings telegramSettings, chatID int64, paths []string, outputName string, startIndex, totalParts int, progress func(int64), status func(int, bool)) ([]telegramMessage, error) {
	if len(paths) < 2 || len(paths) > telegramMediaGroupMaxItems {
		return nil, errors.New("Telegram 相册文件段数量必须为 2 至 10")
	}
	staged := make([]telegramSentVideo, 0, len(paths))
	defer func() {
		for _, message := range staged {
			_ = s.call(context.Background(), settings, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": message.MessageID}, nil)
		}
	}()
	fileIDs := make([]string, 0, len(paths))
	for index, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, errors.New("读取待上传文件失败")
		}
		partNumber := startIndex + index
		var transmitted int64
		waitingReported := false
		partProgress := func(delta int64) {
			if progress != nil {
				progress(delta)
			}
			transmitted += delta
			if !waitingReported && transmitted >= info.Size() {
				waitingReported = true
				if status != nil {
					status(partNumber, false)
				}
			}
		}
		var sent telegramSentVideo
		caption := fmt.Sprintf("%s (%d/%d)", outputName, partNumber, totalParts)
		if err := s.sendFileResult(ctx, settings, "sendVideo", chatID, "video", path, caption, partProgress, &sent); err != nil {
			return nil, err
		}
		if sent.MessageID == 0 || sent.Video.FileID == "" {
			return nil, errors.New("Bot API 未返回暂存视频的 file_id")
		}
		staged = append(staged, sent)
		fileIDs = append(fileIDs, sent.Video.FileID)
		if status != nil {
			status(partNumber, true)
		}
	}
	return s.sendMediaGroupByFileIDs(ctx, settings, chatID, fileIDs, outputName, startIndex, totalParts)
}

func (s *telegramService) sendMediaGroupByFileIDs(ctx context.Context, settings telegramSettings, chatID int64, fileIDs []string, outputName string, startIndex, totalParts int) ([]telegramMessage, error) {
	media := make([]telegramInputMedia, 0, len(fileIDs))
	for index, fileID := range fileIDs {
		item := telegramInputMedia{Type: "video", Media: fileID}
		if index == len(fileIDs)-1 {
			item.Caption = fmt.Sprintf("%s (%d/%d)", outputName, startIndex, totalParts)
		}
		media = append(media, item)
	}
	var sent []telegramMessage
	if err := s.call(ctx, settings, "sendMediaGroup", map[string]any{"chat_id": chatID, "media": media}, &sent); err != nil {
		return nil, err
	}
	return sent, nil
}

func (s *telegramService) sendMediaGroupByItems(ctx context.Context, settings telegramSettings, chatID int64, items []telegramAlbumItem, caption string) ([]telegramMessage, error) {
	if len(items) < 2 || len(items) > telegramMediaGroupMaxItems {
		return nil, errors.New("Telegram 相册媒体数量必须为 2 至 10")
	}
	media := make([]telegramInputMedia, 0, len(items))
	for index, source := range items {
		if (source.Type != "photo" && source.Type != "video") || source.FileID == "" {
			return nil, errors.New("Telegram 相册媒体数据无效")
		}
		item := telegramInputMedia{Type: source.Type, Media: source.FileID}
		if index == len(items)-1 {
			item.Caption = caption
		}
		media = append(media, item)
	}
	var sent []telegramMessage
	if err := s.call(ctx, settings, "sendMediaGroup", map[string]any{"chat_id": chatID, "media": media}, &sent); err != nil {
		return nil, err
	}
	return sent, nil
}

func writeTelegramFileForm(form *multipart.Writer, chatID int64, field, path, caption string, source io.Reader, progress func(int64)) error {
	if err := form.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if err := form.WriteField("caption", caption); err != nil {
		return err
	}
	part, err := form.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, telegramProgressReader{source: source, report: progress}); err != nil {
		return err
	}
	return form.Close()
}

func writeTelegramMediaGroupForm(form *multipart.Writer, chatID int64, paths []string, outputName string, startIndex, totalParts int, progress func(int64)) error {
	if err := form.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	media := make([]telegramInputMedia, 0, len(paths))
	for index := range paths {
		item := telegramInputMedia{Type: "video", Media: fmt.Sprintf("attach://file%d", index)}
		if index == len(paths)-1 {
			item.Caption = fmt.Sprintf("%s (%d/%d)", outputName, startIndex, totalParts)
		}
		media = append(media, item)
	}
	encodedMedia, err := json.Marshal(media)
	if err != nil {
		return err
	}
	if err := form.WriteField("media", string(encodedMedia)); err != nil {
		return err
	}
	for index, path := range paths {
		source, err := os.Open(path)
		if err != nil {
			return errors.New("读取待上传文件失败")
		}
		part, createErr := form.CreateFormFile(fmt.Sprintf("file%d", index), filepath.Base(path))
		if createErr == nil {
			_, createErr = io.Copy(part, telegramProgressReader{source: source, report: progress})
		}
		closeErr := source.Close()
		if createErr != nil {
			return createErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return form.Close()
}

type telegramProgressReader struct {
	source io.Reader
	report func(int64)
}

func (r telegramProgressReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	if count > 0 && r.report != nil {
		r.report(int64(count))
	}
	return count, err
}

func (s *telegramService) sendTelegramRequest(request *http.Request, result any) error {
	return s.sendTelegramRequestWithClient(s.client, request, result)
}

func (s *telegramService) sendTelegramUploadRequest(request *http.Request, result any) error {
	return s.sendTelegramRequestWithClient(s.uploadClient, request, result)
}

func (s *telegramService) sendTelegramRequestWithClient(client *http.Client, request *http.Request, result any) error {
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return telegramTransportError(err)
	}
	defer response.Body.Close()
	var decoded telegramResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&decoded); err != nil {
		return errors.New("Bot API 返回无效响应")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || !decoded.OK {
		if decoded.Description != "" {
			return errors.New(decoded.Description)
		}
		return fmt.Errorf("Bot API 返回 HTTP %d", response.StatusCode)
	}
	if result != nil && len(decoded.Result) > 0 {
		if err := json.Unmarshal(decoded.Result, result); err != nil {
			return errors.New("Bot API 返回数据无效")
		}
	}
	return nil
}

func telegramTransportError(err error) error {
	var requestError *url.Error
	if errors.As(err, &requestError) && requestError.Err != nil {
		err = requestError.Err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("本地 Bot API Server 在返回确认前断开连接，上传结果未知（Telegram 可能仍在后台完成），请先检查目标会话并避免立即重试: %w", err)
	}
	return fmt.Errorf("连接本地 Bot API Server 失败: %w", err)
}
