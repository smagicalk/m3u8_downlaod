package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
)

const telegramAlbumCollectDelay = 750 * time.Millisecond

var telegramAlbumPartCaption = regexp.MustCompile(`\s+\(\d+/\d+\)$`)

type telegramAlbum struct {
	ChatID       int64
	MediaGroupID string
	TaskID       string
	OutputName   string
	Caption      string
	Items        []telegramAlbumItem
}

type telegramAlbumItem struct {
	MessageID int
	Type      string
	FileID    string
}

type telegramAlbumCollection struct {
	Key                string
	ChatID             int64
	TargetMediaGroupID string
	PhotoFileIDs       []string
	SourceMessageIDs   []int
	Settings           telegramSettings
	Timer              *time.Timer
}

type telegramAlbumEditSession struct {
	ChatID             int64
	TargetMediaGroupID string
}

type telegramAlbumAddition struct {
	Token                 string
	ChatID                int64
	TargetMediaGroupID    string
	PhotoFileIDs          []string
	SourceMessageIDs      []int
	Caption               string
	UpdateCaption         bool
	ConfirmationMessageID int
}

type telegramAlbumImportCollection struct {
	Key      string
	Album    telegramAlbum
	Settings telegramSettings
	Timer    *time.Timer
}

type telegramAlbumImport struct {
	Token string
	Album telegramAlbum
}

func telegramMergeAlbumItems(current []telegramAlbumItem, newPhotoFileIDs []string) ([]telegramAlbumItem, error) {
	if len(current)+len(newPhotoFileIDs) > telegramMediaGroupMaxItems {
		return nil, errors.New("图片和视频合计不能超过 10 项")
	}
	items := make([]telegramAlbumItem, 0, len(current)+len(newPhotoFileIDs))
	for _, item := range current {
		if item.Type == "photo" {
			items = append(items, telegramAlbumItem{Type: item.Type, FileID: item.FileID})
		}
	}
	for _, fileID := range newPhotoFileIDs {
		if fileID != "" {
			items = append(items, telegramAlbumItem{Type: "photo", FileID: fileID})
		}
	}
	for _, item := range current {
		if item.Type == "video" {
			items = append(items, telegramAlbumItem{Type: item.Type, FileID: item.FileID})
		}
	}
	if len(items) < 2 {
		return nil, errors.New("Telegram 相册至少需要 2 项媒体")
	}
	return items, nil
}

func telegramAlbumFromMessages(chatID int64, taskID, outputName string, messages []telegramMessage) (*telegramAlbum, error) {
	if len(messages) < 2 {
		return nil, errors.New("Bot API 未返回完整的相册消息")
	}
	album := &telegramAlbum{ChatID: chatID, MediaGroupID: messages[0].MediaGroupID, TaskID: taskID, OutputName: outputName, Caption: outputName, Items: make([]telegramAlbumItem, 0, len(messages))}
	if album.MediaGroupID == "" {
		return nil, errors.New("Bot API 未返回 media_group_id")
	}
	for _, message := range messages {
		if message.MediaGroupID != album.MediaGroupID || message.MessageID == 0 {
			return nil, errors.New("Bot API 返回的相册消息不完整")
		}
		item := telegramAlbumItem{MessageID: message.MessageID}
		switch {
		case message.Video != nil && message.Video.FileID != "":
			item.Type, item.FileID = "video", message.Video.FileID
		case len(message.Photo) > 0:
			item.Type, item.FileID = "photo", telegramLargestPhotoFileID(message.Photo)
		}
		if item.FileID == "" {
			return nil, errors.New("Bot API 未返回相册媒体的 file_id")
		}
		album.Items = append(album.Items, item)
	}
	return album, nil
}

func telegramLargestPhotoFileID(photos []telegramPhotoSize) string {
	var fileID string
	var largest int64
	for _, photo := range photos {
		size := photo.FileSize
		if size <= 0 {
			size = int64(photo.Width) * int64(photo.Height)
		}
		if fileID == "" || size >= largest {
			fileID, largest = photo.FileID, size
		}
	}
	return fileID
}

func (s *telegramService) saveSentTelegramAlbum(chatID int64, taskID, outputName string, messages []telegramMessage) error {
	if s.store == nil {
		return nil
	}
	album, err := telegramAlbumFromMessages(chatID, taskID, outputName, messages)
	if err != nil {
		return err
	}
	return s.store.saveTelegramAlbum(*album)
}

func (s *telegramService) handleAlbumPhotoMessage(ctx context.Context, settings telegramSettings, message *telegramMessage) bool {
	if message == nil || len(message.Photo) == 0 || s.store == nil {
		return false
	}
	photoFileID := telegramLargestPhotoFileID(message.Photo)
	if photoFileID == "" {
		return false
	}
	key := telegramAlbumCollectionKey(message.Chat.ID, message.MediaGroupID, message.MessageID)
	s.mu.Lock()
	collection := s.albumCollections[key]
	if collection == nil {
		session := s.albumEdits[message.Chat.ID]
		if session == nil {
			s.mu.Unlock()
			return false
		}
		if s.albumCollections == nil {
			s.albumCollections = make(map[string]*telegramAlbumCollection)
		}
		collection = &telegramAlbumCollection{Key: key, ChatID: message.Chat.ID, TargetMediaGroupID: session.TargetMediaGroupID, Settings: cloneTelegramSettings(settings)}
		s.albumCollections[key] = collection
		delete(s.albumEdits, message.Chat.ID)
	}
	if s.albumCollections[key] != collection {
		s.mu.Unlock()
		return true
	}
	if !telegramContainsMessageID(collection.SourceMessageIDs, message.MessageID) {
		collection.PhotoFileIDs = append(collection.PhotoFileIDs, photoFileID)
		collection.SourceMessageIDs = append(collection.SourceMessageIDs, message.MessageID)
	}
	if collection.Timer != nil {
		collection.Timer.Stop()
	}
	collection.Timer = time.AfterFunc(telegramAlbumCollectDelay, func() { s.finishAlbumPhotoCollection(key) })
	s.mu.Unlock()
	return true
}

func (s *telegramService) handleAlbumEditMessage(ctx context.Context, settings telegramSettings, message *telegramMessage) bool {
	if message == nil || s.store == nil {
		return false
	}
	text := strings.TrimSpace(message.Text)
	command := ""
	if fields := strings.Fields(text); len(fields) > 0 {
		candidate := strings.TrimSpace(strings.Split(fields[0], "@")[0])
		if strings.HasPrefix(candidate, "/") {
			command = candidate
		}
	}
	if command == "/add" {
		s.beginAlbumEditSession(ctx, settings, message)
		return true
	}
	s.mu.RLock()
	session := s.albumEdits[message.Chat.ID]
	s.mu.RUnlock()
	if session == nil || text == "" {
		return false
	}
	if command == "/cancel" {
		s.cancelAlbumEditSession(ctx, settings, message.Chat.ID, 0)
		return true
	}
	if command != "" {
		return false
	}
	s.mu.Lock()
	if s.albumEdits[message.Chat.ID] != session {
		s.mu.Unlock()
		return true
	}
	delete(s.albumEdits, message.Chat.ID)
	s.mu.Unlock()
	s.requestAlbumCaptionConfirmation(ctx, settings, message.Chat.ID, session.TargetMediaGroupID, message.MessageID, text)
	return true
}

func (s *telegramService) beginAlbumEditSession(ctx context.Context, settings telegramSettings, message *telegramMessage) {
	if message.ReplyToMessage == nil || message.ReplyToMessage.MediaGroupID == "" {
		_ = s.sendMessage(ctx, settings, message.Chat.ID, "请引用目标相册中的任意视频或图片，再发送 /add。", nil)
		return
	}
	album, err := s.store.loadTelegramAlbum(message.Chat.ID, message.ReplyToMessage.MediaGroupID)
	if err != nil {
		_ = s.sendMessage(ctx, settings, message.Chat.ID, "读取相册信息失败，请稍后重试。", nil)
		return
	}
	if album == nil {
		_ = s.sendMessage(ctx, settings, message.Chat.ID, "未找到该相册的编辑记录；请先使用“导入相册”登记旧相册。", nil)
		return
	}
	session := &telegramAlbumEditSession{ChatID: message.Chat.ID, TargetMediaGroupID: album.MediaGroupID}
	s.mu.Lock()
	if s.albumEdits == nil {
		s.albumEdits = make(map[int64]*telegramAlbumEditSession)
	}
	s.albumEdits[message.Chat.ID] = session
	s.mu.Unlock()
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "取消编辑", CallbackData: "album-edit-cancel"}}}}
	if err := s.sendMessage(ctx, settings, message.Chat.ID, "请发送要添加的图片集合，或直接发送新的统一说明文字。", keyboard); err != nil {
		s.mu.Lock()
		if s.albumEdits[message.Chat.ID] == session {
			delete(s.albumEdits, message.Chat.ID)
		}
		s.mu.Unlock()
	}
}

func (s *telegramService) cancelAlbumEditSession(ctx context.Context, settings telegramSettings, chatID int64, messageID int) {
	s.mu.Lock()
	delete(s.albumEdits, chatID)
	for key, collection := range s.albumCollections {
		if collection.ChatID != chatID {
			continue
		}
		if collection.Timer != nil {
			collection.Timer.Stop()
		}
		delete(s.albumCollections, key)
	}
	s.mu.Unlock()
	if messageID == 0 {
		_ = s.sendMessage(ctx, settings, chatID, "已取消编辑相册。", nil)
		return
	}
	_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "已取消编辑相册。")
}

func (s *telegramService) requestAlbumCaptionConfirmation(ctx context.Context, settings telegramSettings, chatID int64, mediaGroupID string, sourceMessageID int, caption string) {
	album, err := s.store.loadTelegramAlbum(chatID, mediaGroupID)
	if err != nil || album == nil {
		_ = s.sendMessage(ctx, settings, chatID, "无法读取原相册，已取消修改说明。", nil)
		return
	}
	addition := &telegramAlbumAddition{ChatID: chatID, TargetMediaGroupID: mediaGroupID, SourceMessageIDs: []int{sourceMessageID}, Caption: caption, UpdateCaption: true}
	prompt := fmt.Sprintf("确认把《%s》的统一说明修改为：\n\n%s", album.OutputName, caption)
	s.requestAlbumAdditionConfirmation(settings, addition, prompt)
}

func telegramAlbumCollectionKey(chatID int64, mediaGroupID string, messageID int) string {
	if mediaGroupID == "" {
		mediaGroupID = fmt.Sprintf("single-%d", messageID)
	}
	return fmt.Sprintf("%d:%s", chatID, mediaGroupID)
}

func telegramContainsMessageID(items []int, messageID int) bool {
	for _, current := range items {
		if current == messageID {
			return true
		}
	}
	return false
}

func (s *telegramService) finishAlbumPhotoCollection(key string) {
	s.mu.Lock()
	collection := s.albumCollections[key]
	if collection != nil {
		delete(s.albumCollections, key)
	}
	s.mu.Unlock()
	if collection == nil {
		return
	}
	album, err := s.store.loadTelegramAlbum(collection.ChatID, collection.TargetMediaGroupID)
	if err != nil || album == nil {
		_ = s.sendMessage(context.Background(), collection.Settings, collection.ChatID, "无法读取原相册，已取消添加图片。", nil)
		return
	}
	if _, err := telegramMergeAlbumItems(album.Items, collection.PhotoFileIDs); err != nil {
		_ = s.sendMessage(context.Background(), collection.Settings, collection.ChatID, err.Error()+"。", nil)
		return
	}
	addition := &telegramAlbumAddition{ChatID: collection.ChatID, TargetMediaGroupID: collection.TargetMediaGroupID, PhotoFileIDs: append([]string(nil), collection.PhotoFileIDs...), SourceMessageIDs: append([]int(nil), collection.SourceMessageIDs...)}
	text := fmt.Sprintf("确认把 %d 张图片加入《%s》？\n新相册发送成功后才会删除旧相册和这些图片消息。", len(addition.PhotoFileIDs), album.OutputName)
	s.requestAlbumAdditionConfirmation(collection.Settings, addition, text)
}

func (s *telegramService) requestAlbumAdditionConfirmation(settings telegramSettings, addition *telegramAlbumAddition, text string) {
	token := telegramAlbumAdditionToken()
	addition.Token = token
	s.mu.Lock()
	if s.albumAdditions == nil {
		s.albumAdditions = make(map[string]*telegramAlbumAddition)
	}
	s.albumAdditions[token] = addition
	s.mu.Unlock()
	confirmText := "确认添加"
	if addition.UpdateCaption {
		confirmText = "确认修改"
	}
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: confirmText, CallbackData: "album-add-confirm:" + token}, {Text: "取消", CallbackData: "album-add-cancel:" + token}}}}
	var confirmation telegramMessage
	if err := s.sendMessageResult(context.Background(), settings, addition.ChatID, text, keyboard, &confirmation); err != nil {
		s.removeAlbumAddition(token)
		return
	}
	s.mu.Lock()
	if current := s.albumAdditions[token]; current == addition {
		current.ConfirmationMessageID = confirmation.MessageID
	}
	s.mu.Unlock()
}

func telegramAlbumAdditionToken() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func (s *telegramService) handleAlbumCallback(ctx context.Context, settings telegramSettings, chatID int64, messageID int, data string) bool {
	if data == "album-edit-cancel" {
		s.cancelAlbumEditSession(ctx, settings, chatID, messageID)
		return true
	}
	if data == "album-import-mode-cancel" {
		s.cancelAlbumImportMode(ctx, settings, chatID, messageID)
		return true
	}
	if token, found := strings.CutPrefix(data, "album-import-confirm:"); found {
		s.confirmAlbumImport(ctx, settings, chatID, messageID, token)
		return true
	}
	if token, found := strings.CutPrefix(data, "album-import-cancel:"); found {
		s.cancelAlbumImport(ctx, settings, chatID, messageID, token)
		return true
	}
	if token, found := strings.CutPrefix(data, "album-add-confirm:"); found {
		s.confirmAlbumAddition(ctx, settings, chatID, messageID, token)
		return true
	}
	if token, found := strings.CutPrefix(data, "album-add-cancel:"); found {
		s.cancelAlbumAddition(ctx, settings, chatID, messageID, token)
		return true
	}
	return false
}

func (s *telegramService) beginAlbumImportMode(ctx context.Context, settings telegramSettings, chatID int64) {
	s.mu.Lock()
	if s.albumImportModes == nil {
		s.albumImportModes = make(map[int64]struct{})
	}
	s.albumImportModes[chatID] = struct{}{}
	s.mu.Unlock()
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "取消导入", CallbackData: "album-import-mode-cancel"}}}}
	if err := s.sendMessage(ctx, settings, chatID, "请转发需要导入的视频相册。只会接收下一组相册。", keyboard); err != nil {
		s.mu.Lock()
		delete(s.albumImportModes, chatID)
		s.mu.Unlock()
		log.Printf("Telegram 相册导入模式提示发送失败（Chat %d）: %v", chatID, err)
	}
}

func (s *telegramService) cancelAlbumImportMode(ctx context.Context, settings telegramSettings, chatID int64, messageID int) {
	s.mu.Lock()
	delete(s.albumImportModes, chatID)
	for key, collection := range s.albumImports {
		if collection.Album.ChatID != chatID {
			continue
		}
		if collection.Timer != nil {
			collection.Timer.Stop()
		}
		delete(s.albumImports, key)
	}
	s.mu.Unlock()
	_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "已取消导入相册。")
}

func (s *telegramService) handleAlbumImportMessage(ctx context.Context, settings telegramSettings, message *telegramMessage) bool {
	if message == nil || message.Video == nil || message.Video.FileID == "" || message.MediaGroupID == "" || s.store == nil {
		return false
	}
	key := telegramAlbumCollectionKey(message.Chat.ID, message.MediaGroupID, message.MessageID)
	s.mu.Lock()
	if s.albumImports == nil {
		s.albumImports = make(map[string]*telegramAlbumImportCollection)
	}
	collection := s.albumImports[key]
	if collection == nil {
		if _, enabled := s.albumImportModes[message.Chat.ID]; !enabled {
			s.mu.Unlock()
			return false
		}
		delete(s.albumImportModes, message.Chat.ID)
		collection = &telegramAlbumImportCollection{Key: key, Album: telegramAlbum{ChatID: message.Chat.ID, MediaGroupID: message.MediaGroupID}, Settings: cloneTelegramSettings(settings)}
		s.albumImports[key] = collection
	}
	if !telegramContainsMessageID(telegramAlbumMessageIDs(collection.Album.Items), message.MessageID) {
		collection.Album.Items = append(collection.Album.Items, telegramAlbumItem{MessageID: message.MessageID, Type: "video", FileID: message.Video.FileID})
	}
	if strings.TrimSpace(message.Caption) != "" && collection.Album.OutputName == "" {
		collection.Album.OutputName = telegramImportedAlbumOutputName(message.Caption)
		collection.Album.Caption = collection.Album.OutputName
	}
	if collection.Timer != nil {
		collection.Timer.Stop()
	}
	collection.Timer = time.AfterFunc(telegramAlbumCollectDelay, func() { s.finishAlbumImportCollection(key) })
	s.mu.Unlock()
	return true
}

func telegramImportedAlbumOutputName(caption string) string {
	name := strings.TrimSpace(telegramAlbumPartCaption.ReplaceAllString(strings.TrimSpace(caption), ""))
	if name == "" {
		return "导入的视频相册"
	}
	return name
}

func (s *telegramService) finishAlbumImportCollection(key string) {
	s.mu.Lock()
	collection := s.albumImports[key]
	if collection != nil {
		delete(s.albumImports, key)
	}
	s.mu.Unlock()
	if collection == nil {
		return
	}
	if len(collection.Album.Items) < 2 || len(collection.Album.Items) > telegramMediaGroupMaxItems {
		_ = s.sendMessage(context.Background(), collection.Settings, collection.Album.ChatID, "旧相册导入失败：视频数量必须为 2 至 10。", nil)
		return
	}
	if collection.Album.OutputName == "" {
		collection.Album.OutputName = telegramImportedAlbumOutputName("")
	}
	if collection.Album.Caption == "" {
		collection.Album.Caption = collection.Album.OutputName
	}
	if existing, err := s.store.loadTelegramAlbum(collection.Album.ChatID, collection.Album.MediaGroupID); err != nil {
		_ = s.sendMessage(context.Background(), collection.Settings, collection.Album.ChatID, "读取相册记录失败，请稍后重试。", nil)
		return
	} else if existing != nil {
		_ = s.sendMessage(context.Background(), collection.Settings, collection.Album.ChatID, fmt.Sprintf("该相册已经导入，共 %d 个项目。", len(existing.Items)), nil)
		return
	}
	sort.Slice(collection.Album.Items, func(left, right int) bool {
		return collection.Album.Items[left].MessageID < collection.Album.Items[right].MessageID
	})
	token := telegramAlbumAdditionToken()
	pending := &telegramAlbumImport{Token: token, Album: collection.Album}
	s.mu.Lock()
	if s.albumImportConfirmations == nil {
		s.albumImportConfirmations = make(map[string]*telegramAlbumImport)
	}
	s.albumImportConfirmations[token] = pending
	s.mu.Unlock()
	keyboard := telegramInlineKeyboard{InlineKeyboard: [][]telegramInlineButton{{{Text: "确认导入", CallbackData: "album-import-confirm:" + token}, {Text: "取消", CallbackData: "album-import-cancel:" + token}}}}
	text := fmt.Sprintf("检测到《%s》，共 %d 个视频。确认导入后可引用相册发送 /add，添加图片或修改说明。", collection.Album.OutputName, len(collection.Album.Items))
	if err := s.sendMessage(context.Background(), collection.Settings, collection.Album.ChatID, text, keyboard); err != nil {
		s.removeAlbumImport(token)
	}
}

func (s *telegramService) albumImport(token string, chatID int64) *telegramAlbumImport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pending := s.albumImportConfirmations[token]
	if pending == nil || pending.Album.ChatID != chatID {
		return nil
	}
	copy := *pending
	copy.Album.Items = append([]telegramAlbumItem(nil), pending.Album.Items...)
	return &copy
}

func (s *telegramService) removeAlbumImport(token string) {
	s.mu.Lock()
	delete(s.albumImportConfirmations, token)
	s.mu.Unlock()
}

func (s *telegramService) confirmAlbumImport(ctx context.Context, settings telegramSettings, chatID int64, messageID int, token string) {
	pending := s.albumImport(token, chatID)
	if pending == nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "该导入确认已失效。")
		return
	}
	if err := s.store.saveTelegramAlbum(pending.Album); err != nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "旧相册导入失败，请稍后重试。")
		return
	}
	if err := s.editAlbumConfirmation(ctx, settings, chatID, messageID, fmt.Sprintf("已导入 %d 个视频。现在可以引用相册发送 /add 进行编辑。", len(pending.Album.Items))); err != nil {
		log.Printf("Telegram 相册导入结果通知失败（Chat %d，相册 %s）: %v", chatID, pending.Album.MediaGroupID, err)
		return
	}
	s.removeAlbumImport(token)
}

func (s *telegramService) cancelAlbumImport(ctx context.Context, settings telegramSettings, chatID int64, messageID int, token string) {
	pending := s.albumImport(token, chatID)
	if pending == nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "该导入确认已失效。")
		return
	}
	if existing, err := s.store.loadTelegramAlbum(chatID, pending.Album.MediaGroupID); err != nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "读取相册记录失败，请稍后重试。")
		return
	} else if existing != nil {
		if err := s.editAlbumConfirmation(ctx, settings, chatID, messageID, fmt.Sprintf("该相册已导入，共 %d 个视频，无法再取消。", len(existing.Items))); err == nil {
			s.removeAlbumImport(token)
		}
		return
	}
	if err := s.editAlbumConfirmation(ctx, settings, chatID, messageID, "已取消导入旧相册。"); err != nil {
		log.Printf("Telegram 取消相册导入通知失败（Chat %d，相册 %s）: %v", chatID, pending.Album.MediaGroupID, err)
		return
	}
	s.removeAlbumImport(token)
}

func (s *telegramService) takeAlbumAddition(token string, chatID int64) *telegramAlbumAddition {
	s.mu.Lock()
	defer s.mu.Unlock()
	addition := s.albumAdditions[token]
	if addition == nil || addition.ChatID != chatID {
		return nil
	}
	delete(s.albumAdditions, token)
	copy := *addition
	copy.PhotoFileIDs = append([]string(nil), addition.PhotoFileIDs...)
	copy.SourceMessageIDs = append([]int(nil), addition.SourceMessageIDs...)
	return &copy
}

func (s *telegramService) removeAlbumAddition(token string) {
	s.mu.Lock()
	delete(s.albumAdditions, token)
	s.mu.Unlock()
}

func (s *telegramService) cancelAlbumAddition(ctx context.Context, settings telegramSettings, chatID int64, messageID int, token string) {
	addition := s.takeAlbumAddition(token, chatID)
	if addition == nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "该确认已失效。")
		return
	}
	text := "已取消添加图片。"
	if addition.UpdateCaption {
		text = "已取消修改说明。"
	}
	_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, text)
}

func (s *telegramService) confirmAlbumAddition(ctx context.Context, settings telegramSettings, chatID int64, messageID int, token string) {
	addition := s.takeAlbumAddition(token, chatID)
	if addition == nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "该确认已失效。")
		return
	}
	_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "正在重建相册，请稍候……")
	album, err := s.store.loadTelegramAlbum(chatID, addition.TargetMediaGroupID)
	if err != nil || album == nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "原相册记录不存在，未执行修改。")
		return
	}
	items, err := telegramMergeAlbumItems(album.Items, addition.PhotoFileIDs)
	if err != nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, err.Error()+"。")
		return
	}
	caption := album.Caption
	if caption == "" {
		caption = album.OutputName
	}
	if addition.UpdateCaption {
		caption = addition.Caption
	}
	sent, err := s.sendMediaGroupByItems(ctx, settings, chatID, items, caption)
	if err != nil {
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "新相册发送失败，旧相册已保留："+err.Error())
		return
	}
	newAlbum, err := telegramAlbumFromMessages(chatID, album.TaskID, album.OutputName, sent)
	if err != nil {
		_ = telegramDeleteMessages(ctx, s, settings, chatID, telegramMessageIDs(sent))
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "Bot API 返回的新相册信息不完整，旧相册已保留。")
		return
	}
	newAlbum.Caption = caption
	if err := s.store.saveTelegramAlbum(*newAlbum); err != nil {
		telegramDeleteMessages(ctx, s, settings, chatID, telegramMessageIDs(sent))
		_ = s.editAlbumConfirmation(ctx, settings, chatID, messageID, "无法保存新相册记录，旧相册已保留。")
		return
	}
	oldDeleteErr := telegramDeleteMessages(ctx, s, settings, chatID, telegramAlbumMessageIDs(album.Items))
	if oldDeleteErr == nil {
		_ = s.store.deleteTelegramAlbum(chatID, album.MediaGroupID)
	}
	_ = telegramDeleteMessages(ctx, s, settings, chatID, addition.SourceMessageIDs)
	_ = telegramDeleteMessages(ctx, s, settings, chatID, []int{messageID})
	if oldDeleteErr != nil {
		_ = s.sendMessage(ctx, settings, chatID, "新相册已发送，但部分旧相册消息无法删除，请手动清理。", nil)
	}
}

func (s *telegramService) editAlbumConfirmation(ctx context.Context, settings telegramSettings, chatID int64, messageID int, text string) error {
	if err := s.call(ctx, settings, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": telegramInlineKeyboard{}}, nil); err == nil {
		return nil
	} else {
		log.Printf("Telegram 相册确认消息编辑失败（Chat %d，Message %d）: %v；尝试发送新消息", chatID, messageID, err)
		if fallbackErr := s.sendMessage(ctx, settings, chatID, text, nil); fallbackErr != nil {
			return fmt.Errorf("编辑消息失败: %v；发送兜底消息失败: %w", err, fallbackErr)
		}
		return nil
	}
}

func telegramDeleteMessages(ctx context.Context, service *telegramService, settings telegramSettings, chatID int64, messageIDs []int) error {
	var firstErr error
	for _, messageID := range messageIDs {
		if messageID == 0 {
			continue
		}
		if err := service.call(ctx, settings, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func telegramAlbumMessageIDs(items []telegramAlbumItem) []int {
	result := make([]int, 0, len(items))
	for _, item := range items {
		result = append(result, item.MessageID)
	}
	return result
}

func telegramMessageIDs(messages []telegramMessage) []int {
	result := make([]int, 0, len(messages))
	for _, message := range messages {
		result = append(result, message.MessageID)
	}
	return result
}
