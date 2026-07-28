package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsSettingsTaskAndLogs(t *testing.T) {
	storage, generatedPassword, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	if generatedPassword != "" {
		t.Fatal("explicit initial password must not be replaced")
	}
	settings := appSettings{OutputDir: filepath.Join(t.TempDir(), "downloads"), CacheDir: filepath.Join(t.TempDir(), "cache"), DeleteCache: false, WorkerCount: 4}
	if err := storage.saveSettings(settings); err != nil {
		t.Fatal(err)
	}
	loadedSettings, err := storage.loadSettings(appSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if loadedSettings != settings {
		t.Fatalf("settings = %#v, want %#v", loadedSettings, settings)
	}
	now := time.Now().Truncate(time.Millisecond)
	current := &task{ID: "task-1", SourceURL: "https://example.com/video.m3u8", UserAgent: browserUserAgent, Mode: modeDownloadFirst, OutputName: "video.mp4", OutputDir: settings.OutputDir, CacheDir: settings.CacheDir, WorkerCount: 4, CacheKey: "cache-key", OutputPath: filepath.Join(settings.OutputDir, "video.mp4"), Status: statusCompleted, CreatedAt: now, FinishedAt: &now}
	if err := storage.saveTask(current); err != nil {
		t.Fatal(err)
	}
	appendTaskLog(current, "info", "任务已完成")
	if err := storage.saveTaskLog(current.ID, current.LogSequence, current.Logs[0]); err != nil {
		t.Fatal(err)
	}
	items, err := storage.loadTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != statusCompleted || len(items[0].Logs) != 1 || items[0].Logs[0].Message != "任务已完成" {
		t.Fatalf("stored task = %#v", items)
	}
}

func TestAuthenticationProtectsAPIsAndInvalidatesOldPassword(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	settings := appSettings{OutputDir: t.TempDir(), CacheDir: t.TempDir(), DeleteCache: true, WorkerCount: 8}
	manager := newTaskManagerWithStore(settings, storage)
	telegram, err := newTelegramService(storage, manager)
	if err != nil {
		t.Fatal(err)
	}
	handler := newAPIHandler(manager, newAuthService(storage), telegram, http.FileServer(http.Dir("web")))

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized task status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	login := func(password string) *http.Response {
		body, _ := json.Marshal(loginRequest{Username: adminUsername, Password: password})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))
		return recorder.Result()
	}
	loggedIn := login("InitialPass123")
	if loggedIn.StatusCode != http.StatusOK || len(loggedIn.Cookies()) != 1 {
		t.Fatalf("login = %d, cookies = %d", loggedIn.StatusCode, len(loggedIn.Cookies()))
	}
	cookie := loggedIn.Cookies()[0]
	allowed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	request.AddCookie(cookie)
	handler.ServeHTTP(allowed, request)
	if allowed.Code != http.StatusOK {
		t.Fatalf("authorized task status = %d, want %d", allowed.Code, http.StatusOK)
	}
	settingsPage := httptest.NewRecorder()
	settingsRequest := httptest.NewRequest(http.MethodGet, "/settings", nil)
	settingsRequest.AddCookie(cookie)
	handler.ServeHTTP(settingsPage, settingsRequest)
	if settingsPage.Code != http.StatusOK || !bytes.Contains(settingsPage.Body.Bytes(), []byte(`id="telegram-form"`)) {
		t.Fatalf("settings page = %d, contains Telegram form = %t", settingsPage.Code, bytes.Contains(settingsPage.Body.Bytes(), []byte(`id="telegram-form"`)))
	}
	manager.tasks["task-1"] = &task{ID: "task-1", OutputName: "video.mp4", Status: statusRunning}
	taskPage := httptest.NewRecorder()
	taskPageRequest := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	taskPageRequest.AddCookie(cookie)
	handler.ServeHTTP(taskPage, taskPageRequest)
	if taskPage.Code != http.StatusOK || !bytes.Contains(taskPage.Body.Bytes(), []byte(`id="task-log"`)) {
		t.Fatalf("task page = %d, contains log view = %t", taskPage.Code, bytes.Contains(taskPage.Body.Bytes(), []byte(`id="task-log"`)))
	}
	taskAPI := httptest.NewRecorder()
	taskAPIRequest := httptest.NewRequest(http.MethodGet, "/api/tasks/task-1", nil)
	taskAPIRequest.AddCookie(cookie)
	handler.ServeHTTP(taskAPI, taskAPIRequest)
	if taskAPI.Code != http.StatusOK || !bytes.Contains(taskAPI.Body.Bytes(), []byte(`"id":"task-1"`)) {
		t.Fatalf("task API = %d: %s", taskAPI.Code, taskAPI.Body.String())
	}

	changeBody, _ := json.Marshal(changePasswordRequest{CurrentPassword: "InitialPass123", NewPassword: "ChangedPass123"})
	change := httptest.NewRecorder()
	changeRequest := httptest.NewRequest(http.MethodPost, "/api/auth/password", bytes.NewReader(changeBody))
	changeRequest.AddCookie(cookie)
	handler.ServeHTTP(change, changeRequest)
	if change.Code != http.StatusOK {
		t.Fatalf("change password = %d: %s", change.Code, change.Body.String())
	}
	expiredSession := httptest.NewRecorder()
	expiredRequest := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	expiredRequest.AddCookie(cookie)
	handler.ServeHTTP(expiredSession, expiredRequest)
	if expiredSession.Code != http.StatusUnauthorized {
		t.Fatalf("previous session status = %d, want %d", expiredSession.Code, http.StatusUnauthorized)
	}
	if response := login("InitialPass123"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if response := login("ChangedPass123"); response.StatusCode != http.StatusOK {
		t.Fatalf("new password status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestResetAdminPassword(t *testing.T) {
	storage, _, err := openStore(filepath.Join(t.TempDir(), databaseFileName), "InitialPass123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.close() })
	if err := storage.resetAdminPassword("ResetPass123"); err != nil {
		t.Fatal(err)
	}
	if storage.verifyPassword(adminUsername, "InitialPass123") {
		t.Fatal("old password must no longer authenticate")
	}
	if !storage.verifyPassword(adminUsername, "ResetPass123") {
		t.Fatal("reset password must authenticate")
	}
}
