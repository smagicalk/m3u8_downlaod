package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const telegramPollTimeout = 25 * time.Second

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

	mu             sync.RWMutex
	settings       telegramSettings
	cancel         context.CancelFunc
	uploading      map[string]struct{}
	uploadProgress map[string]*telegramUploadProgress
	submissions    map[int64]*telegramSubmission
	views          map[telegramTaskView]*telegramTaskSubscription
}

func newTelegramService(storage *store, manager *taskManager) (*telegramService, error) {
	settings, err := storage.loadTelegramSettings()
	if err != nil {
		return nil, err
	}
	return &telegramService{manager: manager, store: storage, client: &http.Client{Timeout: telegramPollTimeout + 10*time.Second}, uploadClient: &http.Client{}, settings: settings, uploading: make(map[string]struct{}), uploadProgress: make(map[string]*telegramUploadProgress), submissions: make(map[int64]*telegramSubmission), views: make(map[telegramTaskView]*telegramTaskSubscription)}, nil
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
