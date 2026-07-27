package main

import (
	"context"
	"errors"
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

func (m *taskManager) create(sourceURL, referer, cookie, userAgent, mode, outputName, outputDir, cacheDir string, deleteCache bool, workerCount int) (*task, error) {
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
	if err := validateWorkerCount(workerCount); err != nil {
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
	if err := os.MkdirAll(resolvedCacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}
	resumeKey := cacheKey(sourceURL, name)
	name = nextAvailableName(resolvedOutputDir, name)
	identifier, err := newTaskID()
	if err != nil {
		return nil, err
	}
	created := &task{
		ID:          identifier,
		SourceURL:   sourceURL,
		Referer:     strings.TrimSpace(referer),
		Cookie:      strings.TrimSpace(cookie),
		UserAgent:   normalizeUserAgent(userAgent),
		Mode:        modeDownloadFirst,
		OutputName:  name,
		OutputDir:   resolvedOutputDir,
		CacheDir:    resolvedCacheDir,
		DeleteCache: deleteCache,
		WorkerCount: workerCount,
		CacheKey:    resumeKey,
		OutputPath:  filepath.Join(resolvedOutputDir, name),
		Status:      statusQueued,
		CreatedAt:   time.Now(),
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
	cacheDir := current.CacheDir
	deleteCache := current.DeleteCache
	sourceURL := current.SourceURL
	referer := current.Referer
	cookie := current.Cookie
	userAgent := current.UserAgent
	resumeKey := current.CacheKey
	workerCount := current.WorkerCount
	m.mu.RUnlock()
	m.setPhase(identifier, "downloading")
	m.addLog(identifier, "info", fmt.Sprintf("开始使用 Go 下载器，分片并发数为 %d", workerCount))
	onProgress := func(completedSegments, totalSegments int, downloadedBytes int64, completedSeconds, totalSeconds float64) {
		m.setDownloadProgress(identifier, completedSegments, totalSegments, downloadedBytes, completedSeconds, totalSeconds)
	}
	onProcess := func(process *os.Process) { m.setProcess(identifier, process) }
	onLog := func(level, message string) { m.addLog(identifier, level, message) }
	defer m.setProcess(identifier, nil)
	result, err := downloadHLS(ctx, hlsDownloadConfig{
		SourceURL:   sourceURL,
		Referer:     referer,
		Cookie:      cookie,
		UserAgent:   userAgent,
		CacheDir:    cacheDir,
		CacheKey:    resumeKey,
		WorkerCount: workerCount,
	}, hlsDownloadCallbacks{
		OnLog:      onLog,
		OnProgress: onProgress,
		WaitForResume: func(waitContext context.Context) error {
			return m.waitForResume(waitContext, identifier)
		},
	})
	if err != nil {
		m.finish(identifier, statusFailed, err.Error())
		return
	}
	if err := m.waitForResume(ctx, identifier); err != nil {
		m.finish(identifier, statusFailed, "任务已取消")
		return
	}
	m.setPhase(identifier, "merging")
	m.addLog(identifier, "info", "分片下载完成，开始由 FFmpeg 合并 MP4")
	if err := runFFmpeg(ctx, result.PlaylistPath, outputPath, "mp4", false, func(seconds float64) {
		m.setMergeProgress(identifier, seconds)
	}, onProcess, onLog); err != nil {
		m.finish(identifier, statusFailed, err.Error())
		return
	}
	if deleteCache {
		if err := os.RemoveAll(result.CachePath); err != nil {
			m.addLog(identifier, "warning", "合并成功，但删除任务缓存失败")
		} else {
			m.addLog(identifier, "info", "合并成功，已删除任务缓存")
		}
	}
	m.finish(identifier, statusCompleted, "")
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

func (m *taskManager) setDownloadProgress(identifier string, completedSegments, totalSegments int, downloadedBytes int64, completedSeconds, totalSeconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil {
		current.CompletedSegments = completedSegments
		current.TotalSegments = totalSegments
		current.DownloadedBytes = downloadedBytes
		current.ProgressSec = completedSeconds
		if totalSeconds > current.DurationSec {
			current.DurationSec = totalSeconds
		}
	}
}

func (m *taskManager) setMergeProgress(identifier string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil {
		current.ProgressSec = seconds
	}
}

func (m *taskManager) waitForResume(ctx context.Context, identifier string) error {
	for {
		m.mu.RLock()
		current := m.tasks[identifier]
		paused := current != nil && current.Status == statusPaused
		m.mu.RUnlock()
		if current == nil {
			return errors.New("任务不存在")
		}
		if !paused {
			return nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
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
	if current == nil || current.Status != statusRunning {
		m.mu.Unlock()
		return false
	}
	if current.Phase == "downloading" {
		current.Status = statusPaused
		appendTaskLog(current, "info", "任务已暂停，当前分片完成后将停止继续下载")
		m.mu.Unlock()
		return true
	}
	if process == nil {
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
	if current == nil || current.Status != statusPaused {
		m.mu.Unlock()
		return false
	}
	if current.Phase == "downloading" {
		current.Status = statusRunning
		appendTaskLog(current, "info", "任务已继续，正在派发剩余分片")
		m.mu.Unlock()
		return true
	}
	if process == nil {
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
