package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const telegramPollTimeout = 25 * time.Second
const telegramTaskRefreshInterval = 3 * time.Second
const telegramMediaGroupMaxItems = 10

const (
	telegramSubmissionSource  = "source"
	telegramSubmissionReferer = "referer"
	telegramSubmissionCookie  = "cookie"
	telegramSubmissionOutput  = "output"
	telegramSubmissionWorkers = "workers"
	telegramSubmissionCache   = "cache"
)

type telegramConfigRequest struct {
	APIBaseURL  string `json:"apiBaseUrl"`
	BotToken    string `json:"botToken"`
	ChatIDs     string `json:"chatIds"`
	AutoUpload  bool   `json:"autoUpload"`
	SplitSizeMB int    `json:"splitSizeMb"`
}

type telegramConfigResponse struct {
	APIBaseURL         string `json:"apiBaseUrl"`
	ChatIDs            string `json:"chatIds"`
	AutoUpload         bool   `json:"autoUpload"`
	SplitSizeMB        int    `json:"splitSizeMb"`
	BotTokenConfigured bool   `json:"botTokenConfigured"`
	Connected          bool   `json:"connected"`
}

type telegramService struct {
	manager      *taskManager
	store        *store
	client       *http.Client
	uploadClient *http.Client

	mu          sync.RWMutex
	settings    telegramSettings
	cancel      context.CancelFunc
	uploading   map[string]struct{}
	submissions map[int64]*telegramSubmission
	views       map[telegramTaskView]*telegramTaskSubscription
}

type telegramSubmission struct {
	Step        string
	SourceURL   string
	Referer     string
	Cookie      string
	OutputName  string
	WorkerCount int
	DeleteCache bool
}

type telegramTaskView struct {
	ChatID    int64
	MessageID int
}

type telegramTaskSubscription struct {
	cancel context.CancelFunc
}

func newTelegramService(storage *store, manager *taskManager) (*telegramService, error) {
	settings, err := storage.loadTelegramSettings()
	if err != nil {
		return nil, err
	}
	return &telegramService{manager: manager, store: storage, client: &http.Client{Timeout: telegramPollTimeout + 10*time.Second}, uploadClient: &http.Client{}, settings: settings, uploading: make(map[string]struct{}), submissions: make(map[int64]*telegramSubmission), views: make(map[telegramTaskView]*telegramTaskSubscription)}, nil
}

func (s *telegramService) start() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	settings := cloneTelegramSettings(s.settings)
	if !telegramConfigured(settings) {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()
	go s.poll(ctx, settings)
}

func (s *telegramService) stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	for _, subscription := range s.views {
		subscription.cancel()
	}
	s.views = make(map[telegramTaskView]*telegramTaskSubscription)
	s.mu.Unlock()
}

func (s *telegramService) configuration() telegramConfigResponse {
	s.mu.RLock()
	settings := cloneTelegramSettings(s.settings)
	s.mu.RUnlock()
	return telegramConfigResponse{APIBaseURL: settings.APIBaseURL, ChatIDs: telegramChatIDsText(settings.ChatIDs), AutoUpload: settings.AutoUpload, SplitSizeMB: settings.SplitSizeMB, BotTokenConfigured: settings.BotToken != "", Connected: telegramConfigured(settings)}
}

func (s *telegramService) updateConfiguration(request telegramConfigRequest) (telegramConfigResponse, error) {
	settings, err := normalizeTelegramConfiguration(request)
	if err != nil {
		return telegramConfigResponse{}, err
	}
	s.mu.RLock()
	if settings.BotToken == "" {
		settings.BotToken = s.settings.BotToken
	}
	s.mu.RUnlock()
	if settings.AutoUpload && !telegramConfigured(settings) {
		return telegramConfigResponse{}, errors.New("启用自动上传前需要配置 Bot Token 和至少一个 Chat ID")
	}
	if err := s.store.saveTelegramSettings(settings); err != nil {
		return telegramConfigResponse{}, fmt.Errorf("保存 Telegram 设置失败: %w", err)
	}
	s.mu.Lock()
	s.settings = settings
	s.mu.Unlock()
	s.start()
	return s.configuration(), nil
}

func (s *telegramService) unbind() error {
	settings := s.currentSettings()
	settings.BotToken = ""
	settings.ChatIDs = nil
	settings.AutoUpload = false
	if err := s.store.saveTelegramSettings(settings); err != nil {
		return fmt.Errorf("解除 Telegram 绑定失败: %w", err)
	}
	s.mu.Lock()
	s.settings = settings
	s.mu.Unlock()
	s.start()
	return nil
}

func normalizeTelegramConfiguration(request telegramConfigRequest) (telegramSettings, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(request.APIBaseURL), "/")
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return telegramSettings{}, errors.New("Bot API 地址必须是有效的 http 或 https 地址")
	}
	chatIDs, err := parseTelegramChatIDs(request.ChatIDs)
	if err != nil {
		return telegramSettings{}, err
	}
	if request.SplitSizeMB < 1 || request.SplitSizeMB > 2000 {
		return telegramSettings{}, errors.New("视频切分大小必须为 1 至 2000 MB")
	}
	token := strings.TrimSpace(request.BotToken)
	if token != "" && !strings.Contains(token, ":") {
		return telegramSettings{}, errors.New("Bot Token 格式无效")
	}
	return telegramSettings{APIBaseURL: baseURL, BotToken: token, ChatIDs: chatIDs, AutoUpload: request.AutoUpload, SplitSizeMB: request.SplitSizeMB}, nil
}

func (s *telegramService) testConnection(ctx context.Context) error {
	settings := s.currentSettings()
	if !telegramConfigured(settings) {
		return errors.New("请先配置 Bot Token 和至少一个 Chat ID")
	}
	var result struct {
		Username string `json:"username"`
	}
	if err := s.call(ctx, settings, "getMe", nil, &result); err != nil {
		return err
	}
	return nil
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
	if _, exists := s.uploading[identifier]; exists {
		s.mu.Unlock()
		return errors.New("该任务正在上传到 Telegram")
	}
	s.uploading[identifier] = struct{}{}
	s.mu.Unlock()
	s.manager.addLog(identifier, "info", "Telegram 上传任务已加入队列")
	go s.upload(identifier, settings)
	return nil
}

func (s *telegramService) upload(identifier string, settings telegramSettings) {
	defer func() {
		s.mu.Lock()
		delete(s.uploading, identifier)
		s.mu.Unlock()
	}()
	current := s.manager.snapshot(identifier)
	if current == nil {
		return
	}
	limitBytes := int64(settings.SplitSizeMB) * 1_000_000
	parts, cleanup, err := splitTelegramVideo(current.OutputPath, limitBytes, current.DurationSec)
	if err != nil {
		s.manager.addLog(identifier, "error", "Telegram 视频切分失败: "+err.Error())
		return
	}
	defer cleanup()
	s.manager.addLog(identifier, "info", fmt.Sprintf("开始上传到 Telegram，共 %d 个文件段", len(parts)))
	context := context.Background()
	for _, chatID := range settings.ChatIDs {
		if len(parts) == 1 {
			if err := s.sendFile(context, settings, "sendVideo", chatID, "video", parts[0], current.OutputName); err != nil {
				s.manager.addLog(identifier, "error", fmt.Sprintf("Telegram 上传失败（Chat %d）: %v", chatID, err))
				return
			}
			s.manager.addLog(identifier, "info", fmt.Sprintf("已上传 Telegram 视频到 Chat %d", chatID))
			continue
		}
		for start := 0; start < len(parts); {
			end := telegramMediaGroupEnd(start, len(parts))
			if err := s.sendMediaGroup(context, settings, chatID, parts[start:end], current.OutputName, start+1, len(parts)); err != nil {
				s.manager.addLog(identifier, "error", fmt.Sprintf("Telegram 相册上传失败（Chat %d，第 %d-%d 段）: %v", chatID, start+1, end, err))
				return
			}
			s.manager.addLog(identifier, "info", fmt.Sprintf("已上传 Telegram 相册文件段 %d-%d/%d 到 Chat %d", start+1, end, len(parts), chatID))
			start = end
		}
	}
	s.manager.addLog(identifier, "info", "Telegram 视频上传完成")
}

func telegramMediaGroupEnd(start, total int) int {
	end := min(start+telegramMediaGroupMaxItems, total)
	if total-end == 1 {
		end--
	}
	return end
}

func (s *telegramService) poll(ctx context.Context, settings telegramSettings) {
	_ = s.call(ctx, settings, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
	var offset int64
	for ctx.Err() == nil {
		var updates []telegramUpdate
		err := s.call(ctx, settings, "getUpdates", map[string]any{"offset": offset, "timeout": int(telegramPollTimeout.Seconds()), "allowed_updates": []string{"message", "channel_post", "callback_query"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("Telegram Bot 长轮询失败: %v", err)
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
				}
			}
			continue
		}
		for _, update := range updates {
			offset = update.UpdateID + 1
			s.handleUpdate(ctx, settings, update)
		}
	}
}

func (s *telegramService) handleUpdate(ctx context.Context, settings telegramSettings, update telegramUpdate) {
	message := update.Message
	if message == nil {
		message = update.ChannelPost
	}
	if message != nil {
		if !telegramChatAllowed(settings, message.Chat.ID) {
			return
		}
		text := strings.TrimSpace(message.Text)
		if s.handleSubmissionMessage(ctx, settings, message.Chat.ID, text) {
			return
		}
		command := ""
		if fields := strings.Fields(text); len(fields) > 0 {
			command = strings.TrimSpace(strings.Split(fields[0], "@")[0])
		}
		switch command {
		case "/start", "/menu", "/tasks", "/completed":
			s.sendTaskList(ctx, settings, message.Chat.ID, command == "/completed")
		case "/submit":
			s.beginSubmission(ctx, settings, message.Chat.ID)
		}
		return
	}
	if update.CallbackQuery == nil || update.CallbackQuery.Message == nil {
		return
	}
	chatID := update.CallbackQuery.Message.Chat.ID
	if !telegramChatAllowed(settings, chatID) {
		return
	}
	_ = s.call(ctx, settings, "answerCallbackQuery", map[string]any{"callback_query_id": update.CallbackQuery.ID}, nil)
	s.handleCallback(ctx, settings, chatID, update.CallbackQuery.Message.MessageID, update.CallbackQuery.Data)
}

func (s *telegramService) handleCallback(ctx context.Context, settings telegramSettings, chatID int64, messageID int, data string) {
	if data == "tasks" || data == "completed" {
		s.stopTaskView(chatID, messageID)
		s.sendTaskList(ctx, settings, chatID, data == "completed")
		return
	}
	if data == "submit" {
		s.beginSubmission(ctx, settings, chatID)
		return
	}
	if data == "submit-cancel" {
		s.clearSubmission(chatID)
		s.sendTaskList(ctx, settings, chatID, false)
		return
	}
	if workerText, found := strings.CutPrefix(data, "submit-worker:"); found {
		workerCount, err := strconv.Atoi(workerText)
		if err != nil || validateWorkerCount(workerCount) != nil || !s.setSubmissionWorkers(chatID, workerCount) {
			return
		}
		keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "合并后删除缓存", CallbackData: "submit-cache:delete"}, {Text: "保留缓存", CallbackData: "submit-cache:keep"}}, {{Text: "取消提交", CallbackData: "submit-cancel"}}}}
		_ = s.sendMessage(ctx, settings, chatID, "请选择缓存策略。", keyboard)
		return
	}
	if cacheMode, found := strings.CutPrefix(data, "submit-cache:"); found {
		if cacheMode == "delete" || cacheMode == "keep" {
			s.finishSubmission(ctx, settings, chatID, cacheMode == "delete")
		}
		return
	}
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return
	}
	identifier := parts[1]
	switch parts[0] {
	case "task", "refresh":
		s.startTaskView(settings, chatID, messageID, identifier)
	case "pause":
		s.manager.pause(identifier)
		s.startTaskView(settings, chatID, messageID, identifier)
	case "resume":
		s.manager.resume(identifier)
		s.startTaskView(settings, chatID, messageID, identifier)
	case "stop", "cancel":
		s.manager.cancel(identifier)
		s.startTaskView(settings, chatID, messageID, identifier)
	case "upload":
		if err := s.queueUpload(identifier); err != nil {
			_ = s.sendMessage(ctx, settings, chatID, err.Error(), nil)
			return
		}
		_ = s.sendMessage(ctx, settings, chatID, "已开始上传，进度会写入网页任务日志。", nil)
	}
}

func (s *telegramService) sendTaskList(ctx context.Context, settings telegramSettings, chatID int64, completedOnly bool) {
	items := s.manager.all()
	sort.Slice(items, func(left, right int) bool { return items[left].CreatedAt.After(items[right].CreatedAt) })
	keyboard := telegramInlineKeyboard{}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "提交下载", CallbackData: "submit"}})
	count := 0
	for _, current := range items {
		if completedOnly && current.Status != statusCompleted {
			continue
		}
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: fmt.Sprintf("%s · %s", current.OutputName, telegramStatusLabel(current.Status)), CallbackData: "task:" + current.ID}})
		count++
		if count == 8 {
			break
		}
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "全部任务", CallbackData: "tasks"}, {Text: "已完成", CallbackData: "completed"}})
	message := "暂无任务"
	if count > 0 {
		message = "选择任务查看详情："
	}
	_ = s.sendMessage(ctx, settings, chatID, message, keyboard)
}

func (s *telegramService) sendTaskDetails(ctx context.Context, settings telegramSettings, chatID int64, identifier string) {
	current := s.manager.snapshot(identifier)
	if current == nil {
		_ = s.sendMessage(ctx, settings, chatID, "任务不存在。", nil)
		return
	}
	text, keyboard := telegramTaskDetails(current)
	var result json.RawMessage
	if err := s.sendMessageResult(ctx, settings, chatID, text, keyboard, &result); err != nil {
		return
	}
	var message telegramMessage
	if err := json.Unmarshal(result, &message); err == nil && message.MessageID > 0 {
		s.startTaskView(settings, chatID, message.MessageID, identifier)
	}
}

func telegramTaskDetails(current *task) (string, telegramInlineKeyboard) {
	text := fmt.Sprintf("%s\n状态：%s\n%s", current.OutputName, telegramStatusLabel(current.Status), telegramTaskProgress(current))
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "刷新", CallbackData: "refresh:" + current.ID}, {Text: "任务列表", CallbackData: "tasks"}}}}
	if current.Status == statusRunning {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "暂停", CallbackData: "pause:" + current.ID}, {Text: "停止", CallbackData: "stop:" + current.ID}})
	}
	if current.Status == statusPaused {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "继续", CallbackData: "resume:" + current.ID}, {Text: "停止", CallbackData: "stop:" + current.ID}})
	}
	if current.Status == statusCompleted {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "上传视频", CallbackData: "upload:" + current.ID}})
	}
	return text, keyboard
}

func (s *telegramService) editTaskDetails(ctx context.Context, settings telegramSettings, chatID int64, messageID int, identifier string) error {
	current := s.manager.snapshot(identifier)
	if current == nil {
		return errors.New("任务不存在")
	}
	text, keyboard := telegramTaskDetails(current)
	return s.call(ctx, settings, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": keyboard}, nil)
}

func (s *telegramService) startTaskView(settings telegramSettings, chatID int64, messageID int, identifier string) {
	if messageID == 0 {
		s.sendTaskDetails(context.Background(), settings, chatID, identifier)
		return
	}
	view := telegramTaskView{ChatID: chatID, MessageID: messageID}
	ctx, cancel := context.WithCancel(context.Background())
	subscription := &telegramTaskSubscription{cancel: cancel}
	s.mu.Lock()
	if previous := s.views[view]; previous != nil {
		previous.cancel()
	}
	s.views[view] = subscription
	s.mu.Unlock()
	go func() {
		defer s.finishTaskView(view, subscription)
		for {
			_ = s.editTaskDetails(ctx, settings, chatID, messageID, identifier)
			current := s.manager.snapshot(identifier)
			if current == nil || current.Status == statusCompleted || current.Status == statusFailed || current.Status == statusCancelled {
				return
			}
			timer := time.NewTimer(telegramTaskRefreshInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *telegramService) stopTaskView(chatID int64, messageID int) {
	if messageID == 0 {
		return
	}
	view := telegramTaskView{ChatID: chatID, MessageID: messageID}
	s.mu.Lock()
	if subscription := s.views[view]; subscription != nil {
		delete(s.views, view)
		subscription.cancel()
	}
	s.mu.Unlock()
}

func (s *telegramService) finishTaskView(view telegramTaskView, subscription *telegramTaskSubscription) {
	s.mu.Lock()
	if s.views[view] == subscription {
		delete(s.views, view)
	}
	s.mu.Unlock()
}

func (s *telegramService) awaitingSubmission(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, waiting := s.submissions[chatID]
	return waiting
}

func (s *telegramService) beginSubmission(ctx context.Context, settings telegramSettings, chatID int64) {
	s.mu.Lock()
	if s.submissions == nil {
		s.submissions = make(map[int64]*telegramSubmission)
	}
	defaults := s.manager.settings()
	s.submissions[chatID] = &telegramSubmission{Step: telegramSubmissionSource, WorkerCount: defaults.WorkerCount, DeleteCache: defaults.DeleteCache}
	s.mu.Unlock()
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "取消提交", CallbackData: "submit-cancel"}}}}
	_ = s.sendMessage(ctx, settings, chatID, "请发送 M3U8 地址。", keyboard)
}

func (s *telegramService) clearSubmission(chatID int64) {
	s.mu.Lock()
	delete(s.submissions, chatID)
	s.mu.Unlock()
}

func (s *telegramService) handleSubmissionMessage(ctx context.Context, settings telegramSettings, chatID int64, text string) bool {
	s.mu.RLock()
	submission := s.submissions[chatID]
	if submission != nil {
		copy := *submission
		submission = &copy
	}
	s.mu.RUnlock()
	if submission == nil {
		return false
	}
	if text == "/cancel" || text == "/menu" || text == "/start" {
		s.clearSubmission(chatID)
		if text != "/cancel" {
			s.sendTaskList(ctx, settings, chatID, false)
		} else {
			_ = s.sendMessage(ctx, settings, chatID, "已取消提交。", nil)
		}
		return true
	}
	if strings.HasPrefix(text, "/") {
		return false
	}
	switch submission.Step {
	case telegramSubmissionSource:
		if err := validateSourceURL(text); err != nil {
			_ = s.sendMessage(ctx, settings, chatID, err.Error()+"。请重新发送 M3U8 地址。", nil)
			return true
		}
		s.updateSubmission(chatID, func(next *telegramSubmission) { next.SourceURL, next.Step = text, telegramSubmissionReferer })
		_ = s.sendMessage(ctx, settings, chatID, "请发送 Referer；不需要请发送 -。", nil)
	case telegramSubmissionReferer:
		value := telegramOptionalValue(text)
		if err := validateReferer(value); err != nil {
			_ = s.sendMessage(ctx, settings, chatID, err.Error()+"。请重新发送 Referer，或发送 - 跳过。", nil)
			return true
		}
		s.updateSubmission(chatID, func(next *telegramSubmission) { next.Referer, next.Step = value, telegramSubmissionCookie })
		_ = s.sendMessage(ctx, settings, chatID, "请发送 Cookie；不需要请发送 -。", nil)
	case telegramSubmissionCookie:
		value := telegramOptionalValue(text)
		if err := validateCookie(value); err != nil {
			_ = s.sendMessage(ctx, settings, chatID, err.Error()+"。请重新发送 Cookie，或发送 - 跳过。", nil)
			return true
		}
		s.updateSubmission(chatID, func(next *telegramSubmission) { next.Cookie, next.Step = value, telegramSubmissionOutput })
		_ = s.sendMessage(ctx, settings, chatID, "请发送输出文件名；使用默认名请发送 -。", nil)
	case telegramSubmissionOutput:
		value := telegramOptionalValue(text)
		if _, err := normalizeOutputName(value); err != nil {
			_ = s.sendMessage(ctx, settings, chatID, err.Error()+"。请重新发送文件名，或发送 - 使用默认名。", nil)
			return true
		}
		s.updateSubmission(chatID, func(next *telegramSubmission) { next.OutputName, next.Step = value, telegramSubmissionWorkers })
		keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "1 线程", CallbackData: "submit-worker:1"}, {Text: "4 线程", CallbackData: "submit-worker:4"}}, {{Text: "8 线程", CallbackData: "submit-worker:8"}, {Text: "16 线程", CallbackData: "submit-worker:16"}}, {{Text: "取消提交", CallbackData: "submit-cancel"}}}}
		_ = s.sendMessage(ctx, settings, chatID, "请选择分片并发数。", keyboard)
	}
	return true
}

func telegramOptionalValue(value string) string {
	if strings.TrimSpace(value) == "-" {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *telegramService) updateSubmission(chatID int64, update func(*telegramSubmission)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.submissions[chatID]
	if current == nil {
		return false
	}
	update(current)
	return true
}

func (s *telegramService) setSubmissionWorkers(chatID int64, workerCount int) bool {
	return s.updateSubmission(chatID, func(current *telegramSubmission) {
		current.WorkerCount, current.Step = workerCount, telegramSubmissionCache
	})
}

func (s *telegramService) finishSubmission(ctx context.Context, settings telegramSettings, chatID int64, deleteCache bool) {
	s.mu.RLock()
	submission := s.submissions[chatID]
	if submission != nil {
		copy := *submission
		submission = &copy
	}
	s.mu.RUnlock()
	if submission == nil || submission.Step != telegramSubmissionCache {
		return
	}
	created, err := s.manager.create(submission.SourceURL, submission.Referer, submission.Cookie, "", modeDownloadFirst, submission.OutputName, "", "", deleteCache, submission.WorkerCount)
	if err != nil {
		_ = s.sendMessage(ctx, settings, chatID, "提交失败："+err.Error()+"。请重新开始提交。", nil)
		return
	}
	s.clearSubmission(chatID)
	_ = s.sendMessage(ctx, settings, chatID, "任务已提交："+created.OutputName, nil)
	s.sendTaskDetails(ctx, settings, chatID, created.ID)
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

func (s *telegramService) sendFile(ctx context.Context, settings telegramSettings, method string, chatID int64, field, path, caption string) error {
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
		if err := writeTelegramFileForm(form, chatID, field, path, caption, source); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	defer reader.Close()
	return s.sendTelegramUploadRequest(request, nil)
}

func (s *telegramService) sendMediaGroup(ctx context.Context, settings telegramSettings, chatID int64, paths []string, outputName string, startIndex, totalParts int) error {
	if len(paths) < 2 || len(paths) > telegramMediaGroupMaxItems {
		return errors.New("Telegram 相册文件段数量必须为 2 至 10")
	}
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(settings.APIBaseURL, "/")+"/bot"+settings.BotToken+"/sendMediaGroup", reader)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	go func() {
		if err := writeTelegramMediaGroupForm(form, chatID, paths, outputName, startIndex, totalParts); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	defer reader.Close()
	return s.sendTelegramUploadRequest(request, nil)
}

func writeTelegramFileForm(form *multipart.Writer, chatID int64, field, path, caption string, source io.Reader) error {
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
	if _, err := io.Copy(part, source); err != nil {
		return err
	}
	return form.Close()
}

func writeTelegramMediaGroupForm(form *multipart.Writer, chatID int64, paths []string, outputName string, startIndex, totalParts int) error {
	if err := form.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	type inputMediaDocument struct {
		Type    string `json:"type"`
		Media   string `json:"media"`
		Caption string `json:"caption,omitempty"`
	}
	media := make([]inputMediaDocument, 0, len(paths))
	for index := range paths {
		item := inputMediaDocument{Type: "document", Media: fmt.Sprintf("attach://file%d", index)}
		if index == 0 {
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
			_, createErr = io.Copy(part, source)
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
		return errors.New("无法连接本地 Bot API Server")
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

func (s *telegramService) currentSettings() telegramSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneTelegramSettings(s.settings)
}

func cloneTelegramSettings(settings telegramSettings) telegramSettings {
	settings.ChatIDs = append([]int64(nil), settings.ChatIDs...)
	return settings
}

func telegramConfigured(settings telegramSettings) bool {
	return settings.BotToken != "" && settings.APIBaseURL != "" && len(settings.ChatIDs) > 0
}

func telegramChatAllowed(settings telegramSettings, identifier int64) bool {
	for _, chatID := range settings.ChatIDs {
		if chatID == identifier {
			return true
		}
	}
	return false
}

func telegramStatusLabel(status taskStatus) string {
	labels := map[taskStatus]string{statusQueued: "等待中", statusRunning: "下载中", statusPaused: "已暂停", statusCompleted: "已完成", statusFailed: "失败", statusCancelled: "已取消"}
	return labels[status]
}

func telegramTaskProgress(current *task) string {
	if current.Phase == "downloading" && current.TotalSegments > 0 {
		return fmt.Sprintf("分片：%d/%d", current.CompletedSegments, current.TotalSegments)
	}
	if current.Phase == "merging" {
		return "正在合并 MP4"
	}
	return "等待处理"
}

type telegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	ChannelPost   *telegramMessage       `json:"channel_post"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramMessage struct {
	MessageID int          `json:"message_id"`
	Chat      telegramChat `json:"chat"`
	Text      string       `json:"text"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	Data    string           `json:"data"`
	Message *telegramMessage `json:"message"`
}

type telegramInlineKeyboard struct {
	InlineKeyboard [][]telegramInlineButton `json:"inline_keyboard"`
}

type telegramInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}
