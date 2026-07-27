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
	handler := newAPIHandler(manager, newAuthService(storage), http.FileServer(http.Dir("web")))

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
