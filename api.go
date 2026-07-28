package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
)

func newAPIHandler(manager *taskManager, auth *authService, telegram *telegramService, staticFiles http.Handler) http.Handler {
	public := http.NewServeMux()
	protected := http.NewServeMux()
	public.HandleFunc("GET /login", serveStaticPage(staticFiles, "/login.html"))
	public.HandleFunc("POST /api/auth/login", func(writer http.ResponseWriter, request *http.Request) {
		if err := auth.login(writer, request); err != nil {
			writeError(writer, http.StatusUnauthorized, err.Error())
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"username": adminUsername})
	})
	protected.HandleFunc("POST /api/auth/logout", func(writer http.ResponseWriter, request *http.Request) {
		auth.logout(writer, request)
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})
	protected.HandleFunc("POST /api/auth/password", func(writer http.ResponseWriter, request *http.Request) {
		var payload changePasswordRequest
		if err := decodeJSONBody(writer, request, 4*1024, &payload); err != nil {
			writeError(writer, http.StatusBadRequest, "请求内容无效")
			return
		}
		if err := auth.store.changePassword(adminUsername, payload.CurrentPassword, payload.NewPassword); err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		auth.invalidateAll()
		auth.logout(writer, request)
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})
	protected.HandleFunc("GET /api/settings/defaults", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, manager.settings())
	})
	protected.HandleFunc("PUT /api/settings/defaults", func(writer http.ResponseWriter, request *http.Request) {
		var payload appSettings
		if err := decodeJSONBody(writer, request, 4*1024, &payload); err != nil {
			writeError(writer, http.StatusBadRequest, "请求内容无效")
			return
		}
		if err := manager.updateSettings(payload); err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(writer, http.StatusOK, manager.settings())
	})
	protected.HandleFunc("GET /api/telegram/config", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, telegram.configuration())
	})
	protected.HandleFunc("PUT /api/telegram/config", func(writer http.ResponseWriter, request *http.Request) {
		var payload telegramConfigRequest
		if err := decodeJSONBody(writer, request, 8*1024, &payload); err != nil {
			writeError(writer, http.StatusBadRequest, "请求内容无效")
			return
		}
		settings, err := telegram.updateConfiguration(payload)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(writer, http.StatusOK, settings)
	})
	protected.HandleFunc("DELETE /api/telegram/config", func(writer http.ResponseWriter, request *http.Request) {
		if err := telegram.unbind(); err != nil {
			writeError(writer, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(writer, http.StatusOK, telegram.configuration())
	})
	protected.HandleFunc("POST /api/telegram/test", func(writer http.ResponseWriter, request *http.Request) {
		if err := telegram.testConnection(request.Context()); err != nil {
			writeError(writer, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})
	protected.HandleFunc("GET /password", serveStaticPage(staticFiles, "/password.html"))
	protected.HandleFunc("GET /settings", serveStaticPage(staticFiles, "/settings.html"))
	protected.HandleFunc("GET /tasks/{id}", serveStaticPage(staticFiles, "/task.html"))
	registerTaskRoutes(protected, manager, telegram)
	protected.Handle("GET /", staticFiles)
	public.Handle("/", auth.require(protected))
	return securityHeaders(public)
}

func registerTaskRoutes(mux *http.ServeMux, manager *taskManager, telegram *telegramService) {
	mux.HandleFunc("POST /api/directories/select", func(writer http.ResponseWriter, request *http.Request) {
		var payload directorySelectionRequest
		if err := decodeJSONBody(writer, request, 4*1024, &payload); err != nil {
			writeError(writer, http.StatusBadRequest, "请求内容无效")
			return
		}
		selected, err := selectDirectory(payload.InitialDirectory)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "无法打开目录选择器")
			return
		}
		writeJSON(writer, http.StatusOK, directorySelectionResponse{Path: selected})
	})
	mux.HandleFunc("GET /api/health", func(writer http.ResponseWriter, request *http.Request) {
		_, err := exec.LookPath(ffmpegExecutable())
		writeJSON(writer, http.StatusOK, map[string]bool{"ffmpegAvailable": err == nil})
	})
	mux.HandleFunc("GET /api/tasks", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, manager.all())
	})
	mux.HandleFunc("GET /api/tasks/{id}", func(writer http.ResponseWriter, request *http.Request) {
		current := manager.snapshot(request.PathValue("id"))
		if current == nil {
			writeError(writer, http.StatusNotFound, "任务不存在")
			return
		}
		writeJSON(writer, http.StatusOK, current)
	})
	mux.HandleFunc("POST /api/tasks", func(writer http.ResponseWriter, request *http.Request) {
		var payload createRequest
		if err := decodeJSONBody(writer, request, 8*1024, &payload); err != nil {
			writeError(writer, http.StatusBadRequest, "请求内容无效")
			return
		}
		defaults := manager.settings()
		workerCount, err := normalizeWorkerCount(payload.WorkerCount, payload.ConcurrentDownloads)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		if payload.WorkerCount == nil && payload.ConcurrentDownloads == nil {
			workerCount = defaults.WorkerCount
		}
		deleteCache := defaults.DeleteCache
		if payload.DeleteCache != nil {
			deleteCache = *payload.DeleteCache
		}
		created, err := manager.create(payload.SourceURL, payload.Referer, payload.Cookie, payload.UserAgent, payload.Mode, payload.OutputName, payload.OutputDir, payload.CacheDir, deleteCache, workerCount)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(writer, http.StatusCreated, created)
	})
	mux.HandleFunc("POST /api/tasks/{id}/cancel", func(writer http.ResponseWriter, request *http.Request) {
		if manager.cancel(request.PathValue("id")) {
			writeJSON(writer, http.StatusOK, manager.snapshot(request.PathValue("id")))
			return
		}
		writeError(writer, http.StatusConflict, "任务不存在或已结束")
	})
	mux.HandleFunc("POST /api/tasks/{id}/pause", func(writer http.ResponseWriter, request *http.Request) {
		if manager.pause(request.PathValue("id")) {
			writeJSON(writer, http.StatusOK, manager.snapshot(request.PathValue("id")))
			return
		}
		writeError(writer, http.StatusConflict, "任务无法暂停")
	})
	mux.HandleFunc("POST /api/tasks/{id}/resume", func(writer http.ResponseWriter, request *http.Request) {
		if manager.resume(request.PathValue("id")) {
			writeJSON(writer, http.StatusOK, manager.snapshot(request.PathValue("id")))
			return
		}
		writeError(writer, http.StatusConflict, "任务无法继续")
	})
	mux.HandleFunc("POST /api/tasks/{id}/telegram-upload", func(writer http.ResponseWriter, request *http.Request) {
		if err := telegram.queueUpload(request.PathValue("id")); err != nil {
			writeError(writer, http.StatusConflict, err.Error())
			return
		}
		writeJSON(writer, http.StatusAccepted, manager.snapshot(request.PathValue("id")))
	})
	mux.HandleFunc("GET /api/tasks/{id}/download", func(writer http.ResponseWriter, request *http.Request) {
		current := manager.snapshot(request.PathValue("id"))
		if current == nil || current.Status != statusCompleted {
			writeError(writer, http.StatusNotFound, "文件不可用")
			return
		}
		if _, err := os.Stat(current.OutputPath); err != nil {
			writeError(writer, http.StatusNotFound, "文件不存在")
			return
		}
		http.ServeFile(writer, request, current.OutputPath)
	})
	mux.HandleFunc("GET /api/proxy/{id}/playlist.m3u8", manager.servePlaylistProxy)
	mux.HandleFunc("GET /api/proxy/{id}/resource/{name}", manager.serveResourceProxy)
}

func decodeJSONBody(writer http.ResponseWriter, request *http.Request, limit int64, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, limit))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func serveStaticPage(staticFiles http.Handler, path string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		originalPath := request.URL.Path
		request.URL.Path = path
		defer func() { request.URL.Path = originalPath }()
		staticFiles.ServeHTTP(writer, request)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}
