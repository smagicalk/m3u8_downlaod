package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

const telegramTaskRefreshInterval = 3 * time.Second
const telegramTaskListPageSize = 8
const telegramCallbackAnswerTimeout = 5 * time.Second

const (
	telegramSubmissionSource  = "source"
	telegramSubmissionReferer = "referer"
	telegramSubmissionCookie  = "cookie"
	telegramSubmissionOutput  = "output"
	telegramSubmissionWorkers = "workers"
	telegramSubmissionCache   = "cache"
)

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

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	ChannelPost   *telegramMessage       `json:"channel_post"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramMessage struct {
	MessageID      int                 `json:"message_id"`
	MediaGroupID   string              `json:"media_group_id"`
	Chat           telegramChat        `json:"chat"`
	Text           string              `json:"text"`
	Caption        string              `json:"caption"`
	Photo          []telegramPhotoSize `json:"photo"`
	Video          *telegramVideo      `json:"video"`
	ReplyToMessage *telegramMessage    `json:"reply_to_message"`
}

type telegramPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type telegramVideo struct {
	FileID string `json:"file_id"`
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

func (s *telegramService) handleUpdate(ctx context.Context, settings telegramSettings, update telegramUpdate) {
	message := update.Message
	if message == nil {
		message = update.ChannelPost
	}
	if message != nil {
		if !telegramChatAllowed(settings, message.Chat.ID) {
			return
		}
		if s.handleAlbumImportMessage(ctx, settings, message) {
			return
		}
		if s.handleAlbumEditMessage(ctx, settings, message) {
			return
		}
		if s.handleAlbumPhotoMessage(ctx, settings, message) {
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
	go s.answerCallbackQuery(settings, update.CallbackQuery.ID)
	s.handleCallback(ctx, settings, chatID, update.CallbackQuery.Message.MessageID, update.CallbackQuery.Data)
}

func (s *telegramService) answerCallbackQuery(settings telegramSettings, callbackQueryID string) {
	ctx, cancel := context.WithTimeout(context.Background(), telegramCallbackAnswerTimeout)
	defer cancel()
	if err := s.call(ctx, settings, "answerCallbackQuery", map[string]any{"callback_query_id": callbackQueryID}, nil); err != nil {
		log.Printf("Telegram 按钮应答失败（Callback %s）: %v", callbackQueryID, err)
	}
}

func (s *telegramService) handleCallback(ctx context.Context, settings telegramSettings, chatID int64, messageID int, data string) {
	if s.handleAlbumCallback(ctx, settings, chatID, messageID, data) {
		return
	}
	if data == "tasks" || data == "completed" {
		s.stopTaskView(chatID, messageID)
		s.editTaskList(ctx, settings, chatID, messageID, data == "completed", 0)
		return
	}
	if completedOnly, page, found := telegramTaskListPageCallback(data); found {
		s.stopTaskView(chatID, messageID)
		s.editTaskList(ctx, settings, chatID, messageID, completedOnly, page)
		return
	}
	if data == "submit" {
		s.beginSubmission(ctx, settings, chatID)
		return
	}
	if data == "album-import" {
		s.beginAlbumImportMode(ctx, settings, chatID)
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
		s.startTaskView(settings, chatID, messageID, identifier)
	}
}

func (s *telegramService) sendTaskList(ctx context.Context, settings telegramSettings, chatID int64, completedOnly bool) {
	text, keyboard := s.taskListPage(completedOnly, 0)
	_ = s.sendMessage(ctx, settings, chatID, text, keyboard)
}

func (s *telegramService) editTaskList(ctx context.Context, settings telegramSettings, chatID int64, messageID int, completedOnly bool, page int) {
	if messageID == 0 {
		s.sendTaskList(ctx, settings, chatID, completedOnly)
		return
	}
	text, keyboard := s.taskListPage(completedOnly, page)
	_ = s.call(ctx, settings, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": keyboard}, nil)
}

func (s *telegramService) taskListPage(completedOnly bool, page int) (string, telegramInlineKeyboard) {
	items := s.manager.all()
	sort.Slice(items, func(left, right int) bool { return items[left].CreatedAt.After(items[right].CreatedAt) })
	filtered := make([]*task, 0, len(items))
	for _, current := range items {
		if !completedOnly || current.Status == statusCompleted {
			filtered = append(filtered, current)
		}
	}
	pageCount := max(1, (len(filtered)+telegramTaskListPageSize-1)/telegramTaskListPageSize)
	page = min(max(page, 0), pageCount-1)
	start := page * telegramTaskListPageSize
	end := min(start+telegramTaskListPageSize, len(filtered))
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "提交下载", CallbackData: "submit"}, {Text: "导入相册", CallbackData: "album-import"}}}}
	for _, current := range filtered[start:end] {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: fmt.Sprintf("%s · %s", current.OutputName, telegramStatusLabel(current.Status)), CallbackData: "task:" + current.ID}})
	}
	if pageCount > 1 {
		mode := "all"
		if completedOnly {
			mode = "completed"
		}
		navigation := make([]telegramInlineButton, 0, 3)
		if page > 0 {
			navigation = append(navigation, telegramInlineButton{Text: "上一页", CallbackData: fmt.Sprintf("tasks-page:%s:%d", mode, page-1)})
		}
		navigation = append(navigation, telegramInlineButton{Text: fmt.Sprintf("第 %d/%d 页", page+1, pageCount), CallbackData: fmt.Sprintf("tasks-page:%s:%d", mode, page)})
		if page+1 < pageCount {
			navigation = append(navigation, telegramInlineButton{Text: "下一页", CallbackData: fmt.Sprintf("tasks-page:%s:%d", mode, page+1)})
		}
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, navigation)
	}
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "全部任务", CallbackData: "tasks"}, {Text: "已完成", CallbackData: "completed"}})
	message := "暂无任务"
	if len(filtered) > 0 {
		message = fmt.Sprintf("选择任务查看详情（第 %d/%d 页）：", page+1, pageCount)
	}
	return message, keyboard
}

func telegramTaskListPageCallback(data string) (completedOnly bool, page int, found bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "tasks-page" {
		return false, 0, false
	}
	page, err := strconv.Atoi(parts[2])
	if err != nil || page < 0 {
		return false, 0, false
	}
	switch parts[1] {
	case "all":
		return false, page, true
	case "completed":
		return true, page, true
	default:
		return false, 0, false
	}
}

func (s *telegramService) sendTaskDetails(ctx context.Context, settings telegramSettings, chatID int64, identifier string) {
	current := s.manager.snapshot(identifier)
	if current == nil {
		_ = s.sendMessage(ctx, settings, chatID, "任务不存在。", nil)
		return
	}
	text, keyboard := s.taskDetails(current)
	var result json.RawMessage
	if err := s.sendMessageResult(ctx, settings, chatID, text, keyboard, &result); err != nil {
		return
	}
	var message telegramMessage
	if err := json.Unmarshal(result, &message); err == nil && message.MessageID > 0 {
		s.startTaskView(settings, chatID, message.MessageID, identifier)
	}
}

func (s *telegramService) taskDetails(current *task) (string, telegramInlineKeyboard) {
	text := fmt.Sprintf("%s\n状态：%s\n%s", current.OutputName, telegramStatusLabel(current.Status), telegramTaskProgress(current))
	progress := s.uploadProgressSnapshot(current.ID)
	if progress != nil {
		text += "\n传入本地 Bot API：" + telegramUploadProgressText(progress)
	}
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "刷新", CallbackData: "refresh:" + current.ID}, {Text: "任务列表", CallbackData: "tasks"}}}}
	if current.Status == statusRunning {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "暂停", CallbackData: "pause:" + current.ID}, {Text: "停止", CallbackData: "stop:" + current.ID}})
	}
	if current.Status == statusPaused {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "继续", CallbackData: "resume:" + current.ID}, {Text: "停止", CallbackData: "stop:" + current.ID}})
	}
	if current.Status == statusCompleted && progress == nil {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []telegramInlineButton{{Text: "上传视频", CallbackData: "upload:" + current.ID}})
	}
	return text, keyboard
}

func (s *telegramService) editTaskDetails(ctx context.Context, settings telegramSettings, chatID int64, messageID int, identifier string) error {
	current := s.manager.snapshot(identifier)
	if current == nil {
		return errors.New("任务不存在")
	}
	text, keyboard := s.taskDetails(current)
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
			if current == nil || ((current.Status == statusCompleted || current.Status == statusFailed || current.Status == statusCancelled) && !s.isUploading(identifier)) {
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
