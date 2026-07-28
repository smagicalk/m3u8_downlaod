package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramSettingsParseAndHideToken(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	manager := newTaskManagerWithStore(appSettings{OutputDir: t.TempDir(), CacheDir: t.TempDir(), DeleteCache: true, WorkerCount: 8}, storage)
	service, err := newTelegramService(storage, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.stop)
	result, err := service.updateConfiguration(telegramConfigRequest{APIBaseURL: "http://127.0.0.1:8081/", BotToken: "123:secret", ChatIDs: "123, -100456,123", AutoUpload: true, SplitSizeMB: 1900})
	if err != nil {
		t.Fatal(err)
	}
	if !result.BotTokenConfigured || result.ChatIDs != "123,-100456" || !result.AutoUpload {
		t.Fatalf("unexpected configuration response: %#v", result)
	}
	stored, err := storage.loadTelegramSettings()
	if err != nil {
		t.Fatal(err)
	}
	if stored.BotToken != "123:secret" || len(stored.ChatIDs) != 2 {
		t.Fatalf("stored Telegram settings = %#v", stored)
	}
	if _, err := parseTelegramChatIDs("invalid"); err == nil {
		t.Fatal("invalid Chat ID must be rejected")
	}
	if err := service.unbind(); err != nil {
		t.Fatal(err)
	}
	if response := service.configuration(); response.Connected || response.BotTokenConfigured || response.ChatIDs != "" {
		t.Fatalf("Telegram binding was not removed: %#v", response)
	}
}

func TestTelegramSegmentDurationUsesTargetSizeRatio(t *testing.T) {
	if got := telegramSegmentDuration(1_000, 100, 225); got != 22.5 {
		t.Fatalf("segment duration = %f, want 22.5", got)
	}
	if got := telegramSegmentDuration(1_000, 100, 1); got != 1 {
		t.Fatalf("minimum segment duration = %f, want 1", got)
	}
}

func TestTelegramSegmentArgumentsProducePlayableMP4(t *testing.T) {
	arguments := strings.Join(telegramSegmentArguments("input.mp4", "segments/part-%03d.mp4", 22.5), " ")
	for _, expected := range []string{"-c copy", "-f segment", "-segment_time 22.500", "-reset_timestamps 1", "-segment_format mp4", "-segment_format_options movflags=+faststart"} {
		if !strings.Contains(arguments, expected) {
			t.Fatalf("segment arguments missing %q: %s", expected, arguments)
		}
	}
}

func TestSplitTelegramVideoCreatesPlayableMP4Segments(t *testing.T) {
	const ffmpegPath = "H:/video/ffmpeg.exe"
	if _, err := exec.LookPath(ffmpegPath); err != nil {
		t.Skip("本机未安装用于集成测试的 FFmpeg")
	}
	previous := ffmpegPathOverride
	ffmpegPathOverride = ffmpegPath
	t.Cleanup(func() { ffmpegPathOverride = previous })
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "source.mp4")
	command := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc=duration=8:size=320x240:rate=30", "-f", "lavfi", "-i", "sine=duration=8", "-c:v", "mpeg4", "-c:a", "aac", "-shortest", inputPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create test video: %v: %s", err, output)
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	parts, cleanup, err := splitTelegramVideo(inputPath, info.Size()/3, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(parts) < 2 {
		t.Fatalf("part count = %d, want at least 2", len(parts))
	}
	for _, part := range parts {
		partInfo, err := os.Stat(part)
		if err != nil {
			t.Fatal(err)
		}
		if partInfo.Size() > info.Size()/3 {
			t.Fatalf("segment %s exceeds size limit: %d", part, partInfo.Size())
		}
		if duration, err := probeTelegramMediaDuration(context.Background(), part); err != nil || duration <= 0 {
			t.Fatalf("segment %s is not independently playable: duration=%f, error=%v", part, duration, err)
		}
	}
}

func TestTelegramFileUploadStreamsMultipartContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.part001")
	if err := os.WriteFile(path, []byte("video-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/sendDocument") {
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
		if !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Fatalf("content type = %q", request.Header.Get("Content-Type"))
		}
		if err := request.ParseMultipartForm(1024 * 1024); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("chat_id") != "42" || request.FormValue("caption") != "video.mp4 (1/3)" {
			t.Fatalf("form values = %#v", request.MultipartForm.Value)
		}
		file, header, err := request.FormFile("document")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if header.Filename != "video.part001" || string(content) != "video-content" {
			t.Fatalf("uploaded file = %q, %q", header.Filename, content)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	service := &telegramService{client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret"}

	if err := service.sendFile(context.Background(), settings, "sendDocument", 42, "document", path, "video.mp4 (1/3)"); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramMediaGroupStreamsVideoAlbum(t *testing.T) {
	directory := t.TempDir()
	parts := []string{filepath.Join(directory, "video.part001"), filepath.Join(directory, "video.part002")}
	for index, path := range parts {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("video-content-%d", index+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/sendMediaGroup") {
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
		if err := request.ParseMultipartForm(1024 * 1024); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("chat_id") != "42" {
			t.Fatalf("chat_id = %q", request.FormValue("chat_id"))
		}
		var media []struct {
			Type    string `json:"type"`
			Media   string `json:"media"`
			Caption string `json:"caption"`
		}
		if err := json.Unmarshal([]byte(request.FormValue("media")), &media); err != nil {
			t.Fatal(err)
		}
		if len(media) != 2 || media[0].Type != "video" || media[0].Media != "attach://file0" || media[1].Media != "attach://file1" || media[0].Caption != "video.mp4 (1/2)" || media[1].Caption != "" {
			t.Fatalf("media = %#v", media)
		}
		for index, field := range []string{"file0", "file1"} {
			file, _, err := request.FormFile(field)
			if err != nil {
				t.Fatal(err)
			}
			content, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(content) != fmt.Sprintf("video-content-%d", index+1) {
				t.Fatalf("file %d content = %q", index+1, content)
			}
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()
	service := &telegramService{client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret"}

	if err := service.sendMediaGroup(context.Background(), settings, 42, parts, "video.mp4", 1, 2); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramMediaGroupDoesNotUsePollTimeout(t *testing.T) {
	directory := t.TempDir()
	parts := []string{filepath.Join(directory, "video.part001"), filepath.Join(directory, "video.part002")}
	for _, path := range parts {
		if err := os.WriteFile(path, []byte("video-content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		time.Sleep(80 * time.Millisecond)
		_, _ = writer.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()
	service := &telegramService{client: &http.Client{Timeout: 10 * time.Millisecond}}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret"}

	if err := service.sendMediaGroup(context.Background(), settings, 42, parts, "video.mp4", 1, 2); err != nil {
		t.Fatalf("large upload must not use the poll timeout: %v", err)
	}
}

func TestTelegramAllowsOnlyBoundChatAndSendsInlineKeyboard(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
		var payload struct {
			ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.ReplyMarkup.InlineKeyboard) == 0 {
			t.Fatal("task menu must include inline keyboard")
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	manager := newTaskManager(t.TempDir())
	manager.tasks["task-1"] = &task{ID: "task-1", OutputName: "video.mp4", Status: statusCompleted}
	service := &telegramService{manager: manager, client: server.Client(), uploading: make(map[string]struct{})}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}, SplitSizeMB: 1900}
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{Chat: telegramChat{ID: 99}, Text: "/start"}})
	if requests.Load() != 0 {
		t.Fatal("unbound chat must not receive a response")
	}
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{Chat: telegramChat{ID: 42}, Text: "/start"}})
	if requests.Load() != 1 {
		t.Fatalf("bound chat requests = %d, want 1", requests.Load())
	}
	service.handleUpdate(context.Background(), settings, telegramUpdate{ChannelPost: &telegramMessage{Chat: telegramChat{ID: 42}, Text: "/tasks"}})
	if requests.Load() != 2 {
		t.Fatalf("channel post requests = %d, want 2", requests.Load())
	}
}

func TestTelegramSubmissionPromptAndTaskControls(t *testing.T) {
	var messages []struct {
		Text        string                 `json:"text"`
		ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
		var payload struct {
			Text        string                 `json:"text"`
			ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, payload)
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	manager := newTaskManager(t.TempDir())
	manager.tasks["task-1"] = &task{ID: "task-1", OutputName: "video.mp4", Status: statusRunning, Phase: "downloading"}
	service := &telegramService{manager: manager, client: server.Client(), uploading: make(map[string]struct{}), submissions: make(map[int64]*telegramSubmission), views: make(map[telegramTaskView]*telegramTaskSubscription)}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}, SplitSizeMB: 1900}

	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{Chat: telegramChat{ID: 42}, Text: "/start"}})
	if len(messages) != 1 || !telegramKeyboardHasCallback(messages[0].ReplyMarkup, "submit") {
		t.Fatal("Telegram menu must include a submit button")
	}
	service.handleCallback(context.Background(), settings, 42, 0, "submit")
	if !service.awaitingSubmission(42) {
		t.Fatal("submit button must put the chat into URL input mode")
	}
	service.handleCallback(context.Background(), settings, 42, 0, "pause:task-1")
	if status := manager.snapshot("task-1").Status; status != statusPaused {
		t.Fatalf("paused task status = %q, want %q", status, statusPaused)
	}
	service.handleCallback(context.Background(), settings, 42, 0, "stop:task-1")
	if status := manager.snapshot("task-1").Status; status != statusCancelled {
		t.Fatalf("stopped task status = %q, want %q", status, statusCancelled)
	}
}

func TestTelegramSubmissionCollectsParametersBeforeCreatingTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	manager := newTaskManager(t.TempDir())
	service := &telegramService{manager: manager, client: server.Client(), uploading: make(map[string]struct{}), submissions: make(map[int64]*telegramSubmission), views: make(map[telegramTaskView]*telegramTaskSubscription)}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}, SplitSizeMB: 1900}

	service.beginSubmission(context.Background(), settings, 42)
	for _, value := range []string{"https://example.com/video.m3u8", "https://example.com/page", "session=value", "custom-video"} {
		if !service.handleSubmissionMessage(context.Background(), settings, 42, value) {
			t.Fatalf("submission message %q was not handled", value)
		}
	}
	service.handleCallback(context.Background(), settings, 42, 0, "submit-worker:4")

	service.mu.RLock()
	submission := *service.submissions[42]
	service.mu.RUnlock()
	if submission.Step != telegramSubmissionCache || submission.WorkerCount != 4 {
		t.Fatalf("submission state = %#v", submission)
	}
	if submission.SourceURL != "https://example.com/video.m3u8" || submission.Referer != "https://example.com/page" || submission.Cookie != "session=value" || submission.OutputName != "custom-video" {
		t.Fatalf("submission parameters = %#v", submission)
	}
}

func TestTelegramTaskRefreshEditsExistingMessage(t *testing.T) {
	var payload struct {
		ChatID    int64  `json:"chat_id"`
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/editMessageText") {
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	manager := newTaskManager(t.TempDir())
	manager.tasks["task-1"] = &task{ID: "task-1", OutputName: "video.mp4", Status: statusRunning, Phase: "downloading"}
	service := &telegramService{manager: manager, client: server.Client(), uploading: make(map[string]struct{}), submissions: make(map[int64]*telegramSubmission), views: make(map[telegramTaskView]*telegramTaskSubscription)}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}, SplitSizeMB: 1900}

	if err := service.editTaskDetails(context.Background(), settings, 42, 77, "task-1"); err != nil {
		t.Fatal(err)
	}
	if payload.ChatID != 42 || payload.MessageID != 77 || !strings.Contains(payload.Text, "video.mp4") {
		t.Fatalf("edited payload = %#v", payload)
	}
}

func telegramKeyboardHasCallback(keyboard telegramInlineKeyboard, callback string) bool {
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if button.CallbackData == callback {
				return true
			}
		}
	}
	return false
}
