package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed web/index.html
var webFiles embed.FS

func main() {
	flag.StringVar(&ffmpegPathOverride, "ffmpeg-path", "", "FFmpeg executable path")
	flag.Parse()

	address := strings.TrimSpace(os.Getenv("ADDR"))
	if address == "" {
		address = defaultAddress
	}

	manager := newTaskManager(downloadDir)
	manager.proxyBaseURL = "http://" + address
	staticFiles, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              address,
		Handler:           newAPIHandler(manager, http.FileServer(http.FS(staticFiles))),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("服务已启动：http://%s", address)
	log.Printf("输出目录：%s", downloadDir)
	log.Fatal(server.ListenAndServe())
}
