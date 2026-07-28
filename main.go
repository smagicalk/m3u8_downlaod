package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web/*.html
var webFiles embed.FS

func main() {
	var resetPassword bool
	flag.StringVar(&ffmpegPathOverride, "ffmpeg-path", "", "FFmpeg executable path")
	flag.BoolVar(&resetPassword, "reset-password", false, "Interactively reset the local admin password")
	flag.Parse()
	if resetPassword {
		if err := runPasswordReset(); err != nil {
			log.Fatal(err)
		}
		return
	}

	address := strings.TrimSpace(os.Getenv("ADDR"))
	if address == "" {
		address = defaultAddress
	}

	fallback, err := defaultSettings()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	storage, generatedPassword, err := openStore(filepath.Join(dataDir, databaseFileName), os.Getenv("M3U8_ADMIN_PASSWORD"))
	if err != nil {
		log.Fatal(err)
	}
	defer storage.close()
	settings, err := storage.loadSettings(fallback)
	if err != nil {
		log.Fatal(err)
	}
	manager := newTaskManagerWithStore(settings, storage)
	items, err := storage.loadTasks()
	if err != nil {
		log.Fatal(err)
	}
	manager.restore(items)
	manager.startCacheCleanup()
	telegram, err := newTelegramService(storage, manager)
	if err != nil {
		log.Fatal(err)
	}
	manager.setCompletionHandler(telegram.onTaskCompleted)
	telegram.start()
	defer telegram.stop()
	manager.proxyBaseURL = "http://" + address
	staticFiles, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              address,
		Handler:           newAPIHandler(manager, newAuthService(storage), telegram, http.FileServer(http.FS(staticFiles))),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("服务已启动：http://%s", address)
	log.Printf("默认输出目录：%s", settings.OutputDir)
	log.Printf("SQLite 数据库：%s", filepath.Join(dataDir, databaseFileName))
	if generatedPassword != "" {
		log.Printf("首次登录账号：admin，临时密码：%s，请登录后立即修改", generatedPassword)
	}
	log.Fatal(server.ListenAndServe())
}
