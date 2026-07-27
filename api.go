package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
)

func newAPIHandler(manager *taskManager, staticFiles http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/directories/select", func(writer http.ResponseWriter, request *http.Request) {
		var payload directorySelectionRequest
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
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
	mux.HandleFunc("POST /api/tasks", func(writer http.ResponseWriter, request *http.Request) {
		var payload createRequest
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 8*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			writeError(writer, http.StatusBadRequest, "请求内容无效")
			return
		}
		workerCount, err := normalizeWorkerCount(payload.WorkerCount, payload.ConcurrentDownloads)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		created, err := manager.create(payload.SourceURL, payload.Referer, payload.Cookie, payload.UserAgent, payload.Mode, payload.OutputName, payload.OutputDir, payload.CacheDir, payload.DeleteCache, workerCount)
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
	mux.Handle("GET /downloads/", http.StripPrefix("/downloads/", http.FileServer(http.Dir(downloadDir))))
	mux.Handle("GET /", staticFiles)
	return securityHeaders(mux)
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
