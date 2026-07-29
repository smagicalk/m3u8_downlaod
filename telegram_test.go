package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestTelegramAPIURLDefaultsFromEnvironmentWithoutOverwritingSavedValue(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_API_URL", "http://telegram-bot-api:8081/")
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	settings, err := storage.loadTelegramSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.APIBaseURL != "http://telegram-bot-api:8081" {
		t.Fatalf("environment API URL = %q", settings.APIBaseURL)
	}
	settings.APIBaseURL = "http://custom-bot-api:9000"
	if err := storage.saveTelegramSettings(settings); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_API_URL", "http://another-default:8081")
	settings, err = storage.loadTelegramSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.APIBaseURL != "http://custom-bot-api:9000" {
		t.Fatalf("saved API URL was overwritten: %q", settings.APIBaseURL)
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

func TestTelegramEffectiveSplitBytesCapsSlowUploads(t *testing.T) {
	if got := telegramEffectiveSplitBytes(1900); got != telegramStableSegmentLimitBytes {
		t.Fatalf("effective split bytes = %d, want %d", got, telegramStableSegmentLimitBytes)
	}
	if got := telegramEffectiveSplitBytes(500); got != 500_000_000 {
		t.Fatalf("effective split bytes = %d, want 500000000", got)
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

	if err := service.sendFile(context.Background(), settings, "sendDocument", 42, "document", path, "video.mp4 (1/3)", nil); err != nil {
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
		if len(media) != 2 || media[0].Type != "video" || media[0].Media != "attach://file0" || media[1].Media != "attach://file1" || media[0].Caption != "" || media[1].Caption != "video.mp4 (1/2)" {
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

	if _, err := service.sendMediaGroup(context.Background(), settings, 42, parts, "video.mp4", 1, 2, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramMediaGroupReportsUploadedMediaBytes(t *testing.T) {
	directory := t.TempDir()
	parts := []string{filepath.Join(directory, "video.part001"), filepath.Join(directory, "video.part002")}
	for index, path := range parts {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("video-content-%d", index+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = writer.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()
	service := &telegramService{uploadClient: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret"}
	var uploaded atomic.Int64

	if _, err := service.sendMediaGroup(context.Background(), settings, 42, parts, "video.mp4", 1, 2, func(delta int64) { uploaded.Add(delta) }); err != nil {
		t.Fatal(err)
	}
	if want := int64(len("video-content-1") + len("video-content-2")); uploaded.Load() != want {
		t.Fatalf("uploaded bytes = %d, want %d", uploaded.Load(), want)
	}
}

func TestTelegramLargeMediaGroupUsesFileIDsAndDeletesStagingMessages(t *testing.T) {
	directory := t.TempDir()
	parts := []string{filepath.Join(directory, "video.part001.mp4"), filepath.Join(directory, "video.part002.mp4")}
	for index, path := range parts {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("video-content-%d", index+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var videoUploads, albums, deletes, waiting, confirmed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/sendVideo"):
			if err := request.ParseMultipartForm(1024 * 1024); err != nil {
				t.Fatal(err)
			}
			index := videoUploads.Add(1)
			_, _ = writer.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"video":{"file_id":"file-id-%d"}}}`, 100+index, index)))
		case strings.HasSuffix(request.URL.Path, "/sendMediaGroup"):
			var payload struct {
				Media []struct {
					Media string `json:"media"`
				} `json:"media"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Media) != 2 || payload.Media[0].Media != "file-id-1" || payload.Media[1].Media != "file-id-2" {
				t.Fatalf("album media = %#v", payload.Media)
			}
			albums.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":[{"message_id":201,"media_group_id":"album-1","video":{"file_id":"file-id-1"}},{"message_id":202,"media_group_id":"album-1","video":{"file_id":"file-id-2"}}]}`))
		case strings.HasSuffix(request.URL.Path, "/deleteMessage"):
			deletes.Add(1)
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	service := &telegramService{client: server.Client(), uploadClient: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret"}
	var uploaded atomic.Int64

	status := func(_ int, isConfirmed bool) {
		if isConfirmed {
			confirmed.Add(1)
		} else {
			waiting.Add(1)
		}
	}
	sent, err := service.sendMediaGroupUsingFileIDs(context.Background(), settings, 42, parts, "video.mp4", 1, 2, func(delta int64) { uploaded.Add(delta) }, status)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].MediaGroupID != "album-1" || sent[1].Video == nil || sent[1].Video.FileID != "file-id-2" {
		t.Fatalf("sent album = %#v", sent)
	}
	if videoUploads.Load() != 2 || albums.Load() != 1 || deletes.Load() != 2 {
		t.Fatalf("requests: videos=%d albums=%d deletes=%d", videoUploads.Load(), albums.Load(), deletes.Load())
	}
	if want := int64(len("video-content-1") + len("video-content-2")); uploaded.Load() != want {
		t.Fatalf("uploaded bytes = %d, want %d", uploaded.Load(), want)
	}
	if waiting.Load() != 2 || confirmed.Load() != 2 {
		t.Fatalf("staging statuses: waiting=%d confirmed=%d", waiting.Load(), confirmed.Load())
	}
}

func TestTelegramLargeMultipartAlbumRequiresFileIDStaging(t *testing.T) {
	if telegramMediaGroupNeedsStaging(telegramMultipartSafeLimitBytes) {
		t.Fatal("safe payload limit must still use direct multipart upload")
	}
	if !telegramMediaGroupNeedsStaging(telegramMultipartSafeLimitBytes + 1) {
		t.Fatal("payload above safe limit must use file_id staging")
	}
}

func TestStorePersistsTelegramAlbum(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	want := telegramAlbum{ChatID: 42, MediaGroupID: "group-1", TaskID: "task-1", OutputName: "video.mp4", Caption: "视频说明", Items: []telegramAlbumItem{{MessageID: 101, Type: "photo", FileID: "photo-1"}, {MessageID: 102, Type: "video", FileID: "video-1"}}}
	if err := storage.saveTelegramAlbum(want); err != nil {
		t.Fatal(err)
	}
	got, err := storage.loadTelegramAlbum(42, "group-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ChatID != want.ChatID || got.MediaGroupID != want.MediaGroupID || got.TaskID != want.TaskID || got.OutputName != want.OutputName || got.Caption != want.Caption || len(got.Items) != 2 || got.Items[0] != want.Items[0] || got.Items[1] != want.Items[1] {
		t.Fatalf("stored album = %#v, want %#v", got, want)
	}
	if err := storage.deleteTelegramAlbum(42, "group-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.loadTelegramAlbum(42, "group-1"); err != nil || got != nil {
		t.Fatalf("deleted album = %#v, error=%v", got, err)
	}
}

func TestStoreMigratesTelegramAlbumCaption(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFileName)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE telegram_albums (chat_id INTEGER NOT NULL, media_group_id TEXT NOT NULL, task_id TEXT NOT NULL, output_name TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (chat_id, media_group_id)); INSERT INTO telegram_albums(chat_id, media_group_id, task_id, output_name, created_at) VALUES(42, 'legacy-album', '', '旧相册说明', 1)`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	storage, _, err := openStore(path, "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	album, err := storage.loadTelegramAlbum(42, "legacy-album")
	if err != nil || album == nil || album.Caption != "旧相册说明" {
		t.Fatalf("migrated album = %#v, error=%v", album, err)
	}
}

func TestTelegramAlbumMergePlacesPhotosBeforeVideos(t *testing.T) {
	current := []telegramAlbumItem{{Type: "photo", FileID: "old-photo"}, {Type: "video", FileID: "video-1"}, {Type: "video", FileID: "video-2"}}
	merged, err := telegramMergeAlbumItems(current, []string{"new-photo-1", "new-photo-2"})
	if err != nil {
		t.Fatal(err)
	}
	want := []telegramAlbumItem{{Type: "photo", FileID: "old-photo"}, {Type: "photo", FileID: "new-photo-1"}, {Type: "photo", FileID: "new-photo-2"}, {Type: "video", FileID: "video-1"}, {Type: "video", FileID: "video-2"}}
	if len(merged) != len(want) {
		t.Fatalf("merged item count = %d, want %d", len(merged), len(want))
	}
	for index := range want {
		if merged[index].Type != want[index].Type || merged[index].FileID != want[index].FileID {
			t.Fatalf("merged[%d] = %#v, want %#v", index, merged[index], want[index])
		}
	}
	tooMany := make([]string, telegramMediaGroupMaxItems-len(current)+1)
	if _, err := telegramMergeAlbumItems(current, tooMany); err == nil {
		t.Fatal("album with more than 10 items must be rejected")
	}
}

func TestTelegramTaskListPaginatesEightTasksPerPage(t *testing.T) {
	manager := newTaskManager(t.TempDir())
	for index := 0; index < 17; index++ {
		identifier := fmt.Sprintf("task-%02d", index)
		manager.tasks[identifier] = &task{ID: identifier, OutputName: fmt.Sprintf("video-%02d.mp4", index), Status: statusCompleted, CreatedAt: time.Unix(int64(index), 0)}
	}
	service := &telegramService{manager: manager}
	text, keyboard := service.taskListPage(false, 1)
	if !strings.Contains(text, "第 2/3 页") {
		t.Fatalf("page text = %q", text)
	}
	if !telegramKeyboardHasCallback(keyboard, "tasks-page:all:0") || !telegramKeyboardHasCallback(keyboard, "tasks-page:all:2") {
		t.Fatalf("page keyboard = %#v", keyboard)
	}
	itemCount := 0
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if strings.HasPrefix(button.CallbackData, "task:") {
				itemCount++
			}
		}
	}
	if itemCount != 8 {
		t.Fatalf("task count on page = %d, want 8", itemCount)
	}
}

func TestTelegramTransportErrorPreservesCauseWithoutToken(t *testing.T) {
	err := telegramTransportError(&url.Error{Op: "Post", URL: "http://127.0.0.1:8081/bot123:secret/sendMediaGroup", Err: errors.New("connection reset by peer")})
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("error must contain connection cause: %v", err)
	}
	if strings.Contains(err.Error(), "123:secret") {
		t.Fatalf("error must not contain bot token: %v", err)
	}
}

func TestTelegramTransportEOFExplainsAmbiguousUploadResult(t *testing.T) {
	err := telegramTransportError(&url.Error{Op: "Post", URL: "http://127.0.0.1:8081/bot123:secret/sendVideo", Err: io.EOF})
	if !strings.Contains(err.Error(), "上传结果未知") || !strings.Contains(err.Error(), "避免立即重试") {
		t.Fatalf("EOF error must explain the ambiguous upload result: %v", err)
	}
	if strings.Contains(err.Error(), "123:secret") {
		t.Fatalf("error must not contain bot token: %v", err)
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

	if _, err := service.sendMediaGroup(context.Background(), settings, 42, parts, "video.mp4", 1, 2, nil); err != nil {
		t.Fatalf("large upload must not use the poll timeout: %v", err)
	}
}

func TestTelegramMediaGroupByItemsSendsPhotosBeforeVideos(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Media []telegramInputMedia `json:"media"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Media) != 3 || payload.Media[0].Type != "photo" || payload.Media[0].Media != "photo-1" || payload.Media[0].Caption != "" || payload.Media[1].Type != "photo" || payload.Media[2].Type != "video" || payload.Media[2].Caption != "video.mp4" {
			t.Fatalf("media payload = %#v", payload.Media)
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":[{"message_id":301,"media_group_id":"new-album","photo":[{"file_id":"photo-1"}]},{"message_id":302,"media_group_id":"new-album","photo":[{"file_id":"photo-2"}]},{"message_id":303,"media_group_id":"new-album","video":{"file_id":"video-1"}}]}`))
	}))
	defer server.Close()
	service := &telegramService{client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret"}
	items := []telegramAlbumItem{{Type: "photo", FileID: "photo-1"}, {Type: "photo", FileID: "photo-2"}, {Type: "video", FileID: "video-1"}}
	sent, err := service.sendMediaGroupByItems(context.Background(), settings, 42, items, "video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[0].MediaGroupID != "new-album" {
		t.Fatalf("sent messages = %#v", sent)
	}
}

func TestTelegramAlbumPhotoConfirmationRebuildsAndReplacesAlbum(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	oldAlbum := telegramAlbum{ChatID: 42, MediaGroupID: "old-album", TaskID: "task-1", OutputName: "video.mp4", Caption: "原说明", Items: []telegramAlbumItem{{MessageID: 101, Type: "video", FileID: "video-1"}, {MessageID: 102, Type: "video", FileID: "video-2"}}}
	if err := storage.saveTelegramAlbum(oldAlbum); err != nil {
		t.Fatal(err)
	}
	confirmation := make(chan string, 1)
	albumPayload := make(chan []telegramInputMedia, 1)
	deleted := make(chan int, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var payload struct {
				ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			for _, row := range payload.ReplyMarkup.InlineKeyboard {
				for _, button := range row {
					if strings.HasPrefix(button.CallbackData, "album-add-confirm:") {
						confirmation <- button.CallbackData
					}
				}
			}
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":900,"chat":{"id":42}}}`))
		case strings.HasSuffix(request.URL.Path, "/sendMediaGroup"):
			var payload struct {
				Media []telegramInputMedia `json:"media"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			albumPayload <- payload.Media
			_, _ = writer.Write([]byte(`{"ok":true,"result":[{"message_id":201,"media_group_id":"new-album","photo":[{"file_id":"photo-1","width":100,"height":100}]},{"message_id":202,"media_group_id":"new-album","photo":[{"file_id":"photo-2","width":100,"height":100}]},{"message_id":203,"media_group_id":"new-album","video":{"file_id":"video-1"}},{"message_id":204,"media_group_id":"new-album","video":{"file_id":"video-2"}}]}`))
		case strings.HasSuffix(request.URL.Path, "/deleteMessage"):
			var payload struct {
				MessageID int `json:"message_id"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			deleted <- payload.MessageID
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(request.URL.Path, "/answerCallbackQuery"):
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(request.URL.Path, "/editMessageText"):
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	service := &telegramService{store: storage, client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 500, Chat: telegramChat{ID: 42}, Text: "/add", ReplyToMessage: &telegramMessage{MessageID: 101, MediaGroupID: "old-album"}}})
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 501, MediaGroupID: "incoming-photos", Chat: telegramChat{ID: 42}, Photo: []telegramPhotoSize{{FileID: "photo-1", Width: 100, Height: 100}}}})
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 502, MediaGroupID: "incoming-photos", Chat: telegramChat{ID: 42}, Photo: []telegramPhotoSize{{FileID: "photo-2", Width: 100, Height: 100}}}})
	var callbackData string
	select {
	case callbackData = <-confirmation:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmation button was not sent")
	}
	service.handleCallback(context.Background(), settings, 42, 900, callbackData)
	select {
	case media := <-albumPayload:
		if len(media) != 4 || media[0].Type != "photo" || media[0].Media != "photo-1" || media[1].Type != "photo" || media[1].Media != "photo-2" || media[2].Type != "video" || media[3].Media != "video-2" {
			t.Fatalf("replacement media = %#v", media)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement album was not sent")
	}
	newAlbum, err := storage.loadTelegramAlbum(42, "new-album")
	if err != nil || newAlbum == nil || len(newAlbum.Items) != 4 {
		t.Fatalf("new album = %#v, error=%v", newAlbum, err)
	}
	if old, err := storage.loadTelegramAlbum(42, "old-album"); err != nil || old != nil {
		t.Fatalf("old album = %#v, error=%v", old, err)
	}
	wantDeleted := map[int]bool{101: true, 102: true, 501: true, 502: true, 900: true}
	for index := 0; index < 5; index++ {
		select {
		case messageID := <-deleted:
			delete(wantDeleted, messageID)
		case <-time.After(time.Second):
			t.Fatalf("messages were not deleted: %#v", wantDeleted)
		}
	}
	if len(wantDeleted) != 0 {
		t.Fatalf("messages were not deleted: %#v", wantDeleted)
	}
}

func TestTelegramAlbumCaptionEditRequiresAddSession(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	album := telegramAlbum{ChatID: 42, MediaGroupID: "old-album", OutputName: "video.mp4", Caption: "原说明", Items: []telegramAlbumItem{{MessageID: 101, Type: "video", FileID: "video-1"}, {MessageID: 102, Type: "video", FileID: "video-2"}}}
	if err := storage.saveTelegramAlbum(album); err != nil {
		t.Fatal(err)
	}
	type sentMessage struct {
		Text        string                 `json:"text"`
		ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
	}
	messages := make(chan sentMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			t.Errorf("unexpected method path: %s", request.URL.Path)
			return
		}
		var payload sentMessage
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		messages <- payload
		_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":900,"chat":{"id":42}}}`))
	}))
	defer server.Close()
	service := &telegramService{store: storage, client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}

	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 700, Chat: telegramChat{ID: 42}, Text: "/add", ReplyToMessage: &telegramMessage{MessageID: 101, MediaGroupID: "old-album"}}})
	select {
	case prompt := <-messages:
		if !strings.Contains(prompt.Text, "图片") || !strings.Contains(prompt.Text, "说明文字") {
			t.Fatalf("edit prompt = %q", prompt.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("/add did not start an album edit session")
	}
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 701, Chat: telegramChat{ID: 42}, Text: "新的统一说明"}})
	select {
	case confirmation := <-messages:
		if !strings.Contains(confirmation.Text, "新的统一说明") || !telegramKeyboardHasPrefix(confirmation.ReplyMarkup, "album-add-confirm:") {
			t.Fatalf("caption confirmation = %#v", confirmation)
		}
	case <-time.After(time.Second):
		t.Fatal("caption edit confirmation was not sent")
	}
}

func TestTelegramAlbumCaptionConfirmationRebuildsWithCaptionOnLastItem(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	oldAlbum := telegramAlbum{ChatID: 42, MediaGroupID: "old-album", OutputName: "video.mp4", Caption: "原说明", Items: []telegramAlbumItem{{MessageID: 101, Type: "video", FileID: "video-1"}, {MessageID: 102, Type: "video", FileID: "video-2"}}}
	if err := storage.saveTelegramAlbum(oldAlbum); err != nil {
		t.Fatal(err)
	}
	mediaPayload := make(chan []telegramInputMedia, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/editMessageText"), strings.HasSuffix(request.URL.Path, "/deleteMessage"):
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(request.URL.Path, "/sendMediaGroup"):
			var payload struct {
				Media []telegramInputMedia `json:"media"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			mediaPayload <- payload.Media
			_, _ = writer.Write([]byte(`{"ok":true,"result":[{"message_id":201,"media_group_id":"new-album","video":{"file_id":"video-1"}},{"message_id":202,"media_group_id":"new-album","video":{"file_id":"video-2"}}]}`))
		default:
			t.Errorf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	addition := &telegramAlbumAddition{Token: "token", ChatID: 42, TargetMediaGroupID: "old-album", Caption: "新的统一说明", UpdateCaption: true, SourceMessageIDs: []int{701}}
	service := &telegramService{store: storage, client: server.Client(), albumAdditions: map[string]*telegramAlbumAddition{"token": addition}}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}

	service.confirmAlbumAddition(context.Background(), settings, 42, 900, "token")
	select {
	case media := <-mediaPayload:
		if len(media) != 2 || media[0].Caption != "" || media[1].Caption != "新的统一说明" {
			t.Fatalf("replacement media = %#v", media)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement album was not sent")
	}
	newAlbum, err := storage.loadTelegramAlbum(42, "new-album")
	if err != nil || newAlbum == nil || newAlbum.Caption != "新的统一说明" {
		t.Fatalf("new album = %#v, error=%v", newAlbum, err)
	}
	if old, err := storage.loadTelegramAlbum(42, "old-album"); err != nil || old != nil {
		t.Fatalf("old album = %#v, error=%v", old, err)
	}
}

func TestTelegramAlbumPhotoCancellationKeepsOriginalMessages(t *testing.T) {
	edited := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/editMessageText") {
			t.Fatalf("cancel must not call %s", request.URL.Path)
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		edited <- payload.Text
		_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	service := &telegramService{client: server.Client(), albumAdditions: map[string]*telegramAlbumAddition{"token": {Token: "token", ChatID: 42, TargetMediaGroupID: "old-album", PhotoFileIDs: []string{"photo-1"}, SourceMessageIDs: []int{501}}}}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}
	service.handleCallback(context.Background(), settings, 42, 900, "album-add-cancel:token")
	select {
	case text := <-edited:
		if text != "已取消添加图片。" {
			t.Fatalf("cancel text = %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel confirmation was not edited")
	}
	if service.takeAlbumAddition("token", 42) != nil {
		t.Fatal("cancelled addition must be removed")
	}
}

func TestTelegramCallbackDoesNotWaitForAcknowledgement(t *testing.T) {
	acknowledgementStarted := make(chan struct{}, 1)
	releaseAcknowledgement := make(chan struct{})
	edited := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/answerCallbackQuery"):
			acknowledgementStarted <- struct{}{}
			<-releaseAcknowledgement
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(request.URL.Path, "/editMessageText"):
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			edited <- payload.Text
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	defer close(releaseAcknowledgement)
	service := &telegramService{client: server.Client(), albumAdditions: map[string]*telegramAlbumAddition{"token": {Token: "token", ChatID: 42}}}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}
	done := make(chan struct{})
	go func() {
		service.handleUpdate(context.Background(), settings, telegramUpdate{CallbackQuery: &telegramCallbackQuery{ID: "callback-1", Data: "album-add-cancel:token", Message: &telegramMessage{MessageID: 900, Chat: telegramChat{ID: 42}}}})
		close(done)
	}()
	select {
	case <-acknowledgementStarted:
	case <-time.After(time.Second):
		t.Fatal("callback acknowledgement was not started")
	}
	select {
	case text := <-edited:
		if text != "已取消添加图片。" {
			t.Fatalf("callback result text = %q", text)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("callback processing waited for answerCallbackQuery")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback processing did not finish")
	}
}

func TestTelegramAlbumImportConfirmationCanRetryAfterNotificationFailure(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	var editRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/editMessageText"):
			requestNumber := editRequests.Add(1)
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if requestNumber == 1 {
				_, _ = writer.Write([]byte(`{"ok":false,"description":"temporary edit failure"}`))
				return
			}
			if !strings.Contains(payload.Text, "已导入 2 个视频") {
				t.Errorf("retry result text = %q", payload.Text)
			}
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			_, _ = writer.Write([]byte(`{"ok":false,"description":"temporary send failure"}`))
		default:
			t.Errorf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	pending := &telegramAlbumImport{Token: "token", Album: telegramAlbum{ChatID: 42, MediaGroupID: "forwarded-album", OutputName: "video.mp4", Items: []telegramAlbumItem{{MessageID: 601, Type: "video", FileID: "video-1"}, {MessageID: 602, Type: "video", FileID: "video-2"}}}}
	service := &telegramService{store: storage, client: server.Client(), albumImportConfirmations: map[string]*telegramAlbumImport{"token": pending}}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}

	service.confirmAlbumImport(context.Background(), settings, 42, 950, "token")
	service.confirmAlbumImport(context.Background(), settings, 42, 950, "token")

	if editRequests.Load() != 2 {
		t.Fatalf("edit requests = %d, want 2", editRequests.Load())
	}
	album, err := storage.loadTelegramAlbum(42, "forwarded-album")
	if err != nil || album == nil || len(album.Items) != 2 {
		t.Fatalf("imported album = %#v, error=%v", album, err)
	}
}

func TestTelegramForwardedAlbumConfirmationImportsSQLite(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	confirmation := make(chan string, 1)
	additionConfirmation := make(chan string, 1)
	edited := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var payload struct {
				ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			for _, row := range payload.ReplyMarkup.InlineKeyboard {
				for _, button := range row {
					if strings.HasPrefix(button.CallbackData, "album-import-confirm:") {
						confirmation <- button.CallbackData
					}
					if strings.HasPrefix(button.CallbackData, "album-add-confirm:") {
						additionConfirmation <- button.CallbackData
					}
				}
			}
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":950,"chat":{"id":42}}}`))
		case strings.HasSuffix(request.URL.Path, "/editMessageText"):
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			edited <- payload.Text
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Fatalf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	service := &telegramService{store: storage, client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}
	service.handleCallback(context.Background(), settings, 42, 940, "album-import")
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 601, MediaGroupID: "forwarded-album", Chat: telegramChat{ID: 42}, Caption: "video.mp4 (1/2)", Video: &telegramVideo{FileID: "video-1"}}})
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 602, MediaGroupID: "forwarded-album", Chat: telegramChat{ID: 42}, Video: &telegramVideo{FileID: "video-2"}}})
	var callbackData string
	select {
	case callbackData = <-confirmation:
	case <-time.After(3 * time.Second):
		t.Fatal("import confirmation button was not sent")
	}
	service.handleCallback(context.Background(), settings, 42, 950, callbackData)
	select {
	case text := <-edited:
		if !strings.Contains(text, "已导入 2 个视频") {
			t.Fatalf("import result text = %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("import confirmation was not updated")
	}
	album, err := storage.loadTelegramAlbum(42, "forwarded-album")
	if err != nil || album == nil {
		t.Fatalf("imported album = %#v, error=%v", album, err)
	}
	if album.OutputName != "video.mp4" || len(album.Items) != 2 || album.Items[0].MessageID != 601 || album.Items[0].FileID != "video-1" || album.Items[1].MessageID != 602 || album.Items[1].FileID != "video-2" {
		t.Fatalf("imported album = %#v", album)
	}
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 603, Chat: telegramChat{ID: 42}, Text: "/add", ReplyToMessage: &telegramMessage{MessageID: 601, MediaGroupID: "forwarded-album"}}})
	service.handleUpdate(context.Background(), settings, telegramUpdate{Message: &telegramMessage{MessageID: 604, Chat: telegramChat{ID: 42}, Photo: []telegramPhotoSize{{FileID: "photo-1", Width: 100, Height: 100}}}})
	select {
	case <-additionConfirmation:
	case <-time.After(3 * time.Second):
		t.Fatal("imported album did not enter the photo confirmation flow")
	}
}

func TestTelegramAlbumImportRequiresExplicitModeAndCanBeCancelled(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	prompted := make(chan telegramInlineKeyboard, 1)
	edited := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var payload struct {
				ReplyMarkup telegramInlineKeyboard `json:"reply_markup"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			prompted <- payload.ReplyMarkup
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":940,"chat":{"id":42}}}`))
		case strings.HasSuffix(request.URL.Path, "/editMessageText"):
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			edited <- payload.Text
			_, _ = writer.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected method path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	service := &telegramService{store: storage, client: server.Client()}
	settings := telegramSettings{APIBaseURL: server.URL, BotToken: "123:secret", ChatIDs: []int64{42}}
	video := &telegramMessage{MessageID: 601, MediaGroupID: "forwarded-album", Chat: telegramChat{ID: 42}, Video: &telegramVideo{FileID: "video-1"}}

	if service.handleAlbumImportMessage(context.Background(), settings, video) {
		t.Fatal("video album must be ignored until import mode is enabled")
	}
	service.handleCallback(context.Background(), settings, 42, 900, "album-import")
	select {
	case keyboard := <-prompted:
		if !telegramKeyboardHasCallback(keyboard, "album-import-mode-cancel") {
			t.Fatal("album import prompt must include a cancel button")
		}
	case <-time.After(time.Second):
		t.Fatal("album import prompt was not sent")
	}
	service.handleCallback(context.Background(), settings, 42, 940, "album-import-mode-cancel")
	select {
	case text := <-edited:
		if text != "已取消导入相册。" {
			t.Fatalf("cancel result text = %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("album import cancellation was not acknowledged")
	}
	if service.handleAlbumImportMessage(context.Background(), settings, video) {
		t.Fatal("video album must be ignored after import mode is cancelled")
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
	if len(messages) != 1 || !telegramKeyboardHasCallback(messages[0].ReplyMarkup, "submit") || !telegramKeyboardHasCallback(messages[0].ReplyMarkup, "album-import") {
		t.Fatal("Telegram menu must include submit and album import buttons")
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

func telegramKeyboardHasPrefix(keyboard telegramInlineKeyboard, prefix string) bool {
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if strings.HasPrefix(button.CallbackData, prefix) {
				return true
			}
		}
	}
	return false
}
