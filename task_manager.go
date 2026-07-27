package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type taskManager struct {
	mu           sync.RWMutex
	tasks        map[string]*task
	cancels      map[string]context.CancelFunc
	processes    map[string]*os.Process
	outputDir    string
	proxyBaseURL string
	httpClient   *http.Client
}

func newTaskManager(outputDir string) *taskManager {
	return &taskManager{
		tasks:      make(map[string]*task),
		cancels:    make(map[string]context.CancelFunc),
		processes:  make(map[string]*os.Process),
		outputDir:  outputDir,
		httpClient: http.DefaultClient,
	}
}

func (m *taskManager) create(sourceURL, referer, cookie, userAgent, mode, outputName, outputDir, cacheDir string, deleteCache, concurrentDownloads bool) (*task, error) {
	if err := validateSourceURL(sourceURL); err != nil {
		return nil, err
	}
	if err := validateReferer(referer); err != nil {
		return nil, err
	}
	if err := validateCookie(cookie); err != nil {
		return nil, err
	}
	if err := validateUserAgent(userAgent); err != nil {
		return nil, err
	}
	if err := validateMode(mode); err != nil {
		return nil, err
	}
	name, err := normalizeOutputName(outputName)
	if err != nil {
		return nil, err
	}
	resolvedOutputDir, err := resolveDirectory(outputDir, m.outputDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(resolvedOutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建下载目录失败: %w", err)
	}
	resolvedCacheDir, err := resolveDirectory(cacheDir, defaultCacheDir)
	if err != nil {
		return nil, err
	}
	if normalizeMode(mode) == modeDownloadFirst {
		if err := os.MkdirAll(resolvedCacheDir, 0o755); err != nil {
			return nil, fmt.Errorf("创建缓存目录失败: %w", err)
		}
	}
	name = nextAvailableName(resolvedOutputDir, name)
	identifier, err := newTaskID()
	if err != nil {
		return nil, err
	}
	created := &task{
		ID:                  identifier,
		SourceURL:           sourceURL,
		Referer:             strings.TrimSpace(referer),
		Cookie:              strings.TrimSpace(cookie),
		UserAgent:           normalizeUserAgent(userAgent),
		Mode:                normalizeMode(mode),
		OutputName:          name,
		OutputDir:           resolvedOutputDir,
		CacheDir:            resolvedCacheDir,
		DeleteCache:         deleteCache,
		ConcurrentDownloads: concurrentDownloads,
		OutputPath:          filepath.Join(resolvedOutputDir, name),
		Status:              statusQueued,
		CreatedAt:           time.Now(),
	}

	m.mu.Lock()
	appendTaskLog(created, "info", "任务已创建，等待开始")
	m.tasks[identifier] = created
	m.mu.Unlock()

	go m.run(identifier)
	return m.snapshot(identifier), nil
}

func (m *taskManager) run(identifier string) {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancels[identifier] = cancel
	current := m.tasks[identifier]
	if current == nil || current.Status == statusCancelled {
		delete(m.cancels, identifier)
		m.mu.Unlock()
		cancel()
		return
	}
	if current.Status == statusQueued {
		started := time.Now()
		current.Status = statusRunning
		current.StartedAt = &started
	}
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, identifier)
		m.mu.Unlock()
	}()

	m.mu.RLock()
	current = m.tasks[identifier]
	if current == nil {
		m.mu.RUnlock()
		return
	}
	outputPath := current.OutputPath
	mode := current.Mode
	cacheDir := current.CacheDir
	deleteCache := current.DeleteCache
	concurrentDownloads := current.ConcurrentDownloads
	m.mu.RUnlock()

	inputURL := m.playlistProxyURL(identifier)
	if inputURL == "" {
		m.addLog(identifier, "error", "本地 HLS 代理未初始化")
		m.finish(identifier, statusFailed, "本地 HLS 代理未初始化")
		return
	}
	m.setPhase(identifier, phaseForMode(mode))
	if concurrentDownloads {
		m.addLog(identifier, "info", "已启用 HLS 多连接分片下载和持久连接")
	} else {
		m.addLog(identifier, "info", "已禁用 HLS 多连接分片下载")
	}
	onProgress := func(seconds float64) {
		m.mu.Lock()
		if current := m.tasks[identifier]; current != nil {
			current.ProgressSec = seconds
		}
		m.mu.Unlock()
	}
	onProcess := func(process *os.Process) { m.setProcess(identifier, process) }
	onLog := func(level, message string) { m.addLog(identifier, level, message) }
	defer m.setProcess(identifier, nil)
	if mode == modeDownloadFirst {
		temporaryPath := filepath.Join(cacheDir, "."+identifier+".ts")
		m.addLog(identifier, "info", "开始下载分片到临时缓存")
		err := runFFmpeg(ctx, inputURL, temporaryPath, "mpegts", concurrentDownloads, onProgress, onProcess, onLog)
		if err == nil {
			m.setPhase(identifier, "merging")
			m.addLog(identifier, "info", "分片下载完成，开始合并 MP4")
			err = runFFmpeg(ctx, temporaryPath, outputPath, "mp4", false, onProgress, onProcess, onLog)
		}
		if err == nil && deleteCache {
			_ = os.Remove(temporaryPath)
			m.addLog(identifier, "info", "合并成功，已删除临时缓存")
		}
		if err != nil {
			m.finish(identifier, statusFailed, err.Error())
			return
		}
	} else {
		m.addLog(identifier, "info", "开始边下载边合并")
		if err := runFFmpeg(ctx, inputURL, outputPath, "mp4", concurrentDownloads, onProgress, onProcess, onLog); err != nil {
			m.finish(identifier, statusFailed, err.Error())
			return
		}
	}
	m.finish(identifier, statusCompleted, "")
}

func phaseForMode(mode string) string {
	if mode == modeDownloadFirst {
		return "downloading"
	}
	return "streaming"
}

func (m *taskManager) setPhase(identifier, phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil && current.Status != statusCancelled {
		current.Status = statusRunning
		current.Phase = phase
	}
}

func (m *taskManager) setProcess(identifier string, process *os.Process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if process == nil {
		delete(m.processes, identifier)
		return
	}
	m.processes[identifier] = process
}

func (m *taskManager) playlistProxyURL(identifier string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.proxyBaseURL == "" {
		return ""
	}
	return m.proxyBaseURL + "/api/proxy/" + url.PathEscape(identifier) + "/playlist.m3u8"
}

func (m *taskManager) finish(identifier string, status taskStatus, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil {
		if current.Status == statusCancelled {
			return
		}
		finished := time.Now()
		current.Status = status
		current.Error = message
		current.FinishedAt = &finished
		if status == statusCompleted {
			appendTaskLog(current, "info", "任务已完成")
		} else if message != "" {
			appendTaskLog(current, "error", message)
		}
	}
}

func (m *taskManager) snapshot(identifier string) *task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.tasks[identifier]
	if current == nil {
		return nil
	}
	copy := *current
	copy.Logs = append([]taskLog(nil), current.Logs...)
	return &copy
}

func (m *taskManager) all() []*task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]*task, 0, len(m.tasks))
	for _, current := range m.tasks {
		copy := *current
		copy.Logs = append([]taskLog(nil), current.Logs...)
		items = append(items, &copy)
	}
	return items
}

func (m *taskManager) cancel(identifier string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.tasks[identifier]
	if current == nil || current.Status != statusQueued && current.Status != statusRunning && current.Status != statusPaused {
		return false
	}
	if current.Status == statusPaused {
		_ = resumeProcess(m.processes[identifier])
	}
	current.Status = statusCancelled
	finished := time.Now()
	current.FinishedAt = &finished
	appendTaskLog(current, "warning", "任务已取消")
	if cancel := m.cancels[identifier]; cancel != nil {
		cancel()
	}
	return true
}

func (m *taskManager) pause(identifier string) bool {
	m.mu.Lock()
	current, process := m.tasks[identifier], m.processes[identifier]
	if current == nil || current.Status != statusRunning || process == nil {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	if err := suspendProcess(process); err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil && current.Status == statusRunning {
		current.Status = statusPaused
		appendTaskLog(current, "info", "任务已暂停")
		return true
	}
	return false
}

func (m *taskManager) resume(identifier string) bool {
	m.mu.Lock()
	current, process := m.tasks[identifier], m.processes[identifier]
	if current == nil || current.Status != statusPaused || process == nil {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	if err := resumeProcess(process); err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil && current.Status == statusPaused {
		current.Status = statusRunning
		appendTaskLog(current, "info", "任务已继续")
		return true
	}
	return false
}
