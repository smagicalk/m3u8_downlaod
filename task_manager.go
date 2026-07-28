package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	store        *store
	defaults     appSettings
	onCompleted  func(*task)
}

func newTaskManager(outputDir string) *taskManager {
	defaults, err := defaultSettings()
	if err == nil {
		defaults.OutputDir = outputDir
	}
	return newTaskManagerWithStore(defaults, nil)
}

func newTaskManagerWithStore(defaults appSettings, storage *store) *taskManager {
	return &taskManager{
		tasks:      make(map[string]*task),
		cancels:    make(map[string]context.CancelFunc),
		processes:  make(map[string]*os.Process),
		outputDir:  defaults.OutputDir,
		httpClient: http.DefaultClient,
		store:      storage,
		defaults:   defaults,
	}
}

func (m *taskManager) settings() appSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaults
}

func (m *taskManager) updateSettings(next appSettings) error {
	outputDir, err := resolveDirectory(next.OutputDir, m.defaults.OutputDir)
	if err != nil {
		return err
	}
	cacheDir, err := resolveDirectory(next.CacheDir, m.defaults.CacheDir)
	if err != nil {
		return err
	}
	if err := validateWorkerCount(next.WorkerCount); err != nil {
		return err
	}
	if err := validateCacheRetentionHours(next.CacheRetentionHours); err != nil {
		return err
	}
	next.OutputDir, next.CacheDir = outputDir, cacheDir
	if err := os.MkdirAll(next.OutputDir, 0o755); err != nil {
		return fmt.Errorf("创建默认保存目录失败: %w", err)
	}
	if err := os.MkdirAll(next.CacheDir, 0o755); err != nil {
		return fmt.Errorf("创建默认缓存目录失败: %w", err)
	}
	if m.store != nil {
		if err := m.store.saveSettings(next); err != nil {
			return fmt.Errorf("保存默认设置失败: %w", err)
		}
	}
	m.mu.Lock()
	m.defaults, m.outputDir = next, next.OutputDir
	m.mu.Unlock()
	m.cleanupExpiredCaches(time.Now())
	return nil
}

func (m *taskManager) restore(items []*task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, current := range items {
		if current.Status == statusQueued || current.Status == statusRunning || current.Status == statusPaused {
			current.Status = statusFailed
			current.Error = "服务重启，任务已中断"
			finished := time.Now()
			current.FinishedAt = &finished
			appendTaskLog(current, "warning", current.Error)
			m.persistTaskLocked(current)
			m.persistLogLocked(current)
		}
		m.tasks[current.ID] = current
	}
}

func (m *taskManager) startCacheCleanup() {
	m.cleanupExpiredCaches(time.Now())
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for now := range ticker.C {
			m.cleanupExpiredCaches(now)
		}
	}()
}

func (m *taskManager) cleanupExpiredCaches(now time.Time) {
	m.mu.RLock()
	retentionHours := m.defaults.CacheRetentionHours
	items := make([]*task, 0, len(m.tasks))
	for _, current := range m.tasks {
		if current.Status != statusCompleted && current.Status != statusFailed && current.Status != statusCancelled {
			continue
		}
		copy := *current
		items = append(items, &copy)
	}
	m.mu.RUnlock()
	if retentionHours == 0 {
		return
	}
	deadline := now.Add(-time.Duration(retentionHours) * time.Hour)
	for _, current := range items {
		if current.FinishedAt == nil || current.FinishedAt.After(deadline) || current.CacheKey == "" {
			continue
		}
		cachePath := filepath.Join(current.CacheDir, "hls-"+current.CacheKey)
		if _, err := os.Stat(cachePath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			m.addLog(current.ID, "warning", "检查过期任务缓存失败")
			continue
		}
		if err := os.RemoveAll(cachePath); err != nil {
			m.addLog(current.ID, "warning", "删除过期任务缓存失败")
			continue
		}
		m.addLog(current.ID, "info", "缓存保留期已结束，已删除任务缓存")
	}
}

func (m *taskManager) setCompletionHandler(handler func(*task)) {
	m.mu.Lock()
	m.onCompleted = handler
	m.mu.Unlock()
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
	defaults := m.settings()
	resolvedOutputDir, err := resolveDirectory(outputDir, defaults.OutputDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(resolvedOutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建下载目录失败: %w", err)
	}
	resolvedCacheDir, err := resolveDirectory(cacheDir, defaults.CacheDir)
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
	m.persistTaskLocked(created)
	m.persistLogLocked(created)
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
	m.persistTaskLocked(current)
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
		m.persistTaskLocked(current)
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
		m.persistTaskLocked(current)
	}
}

func (m *taskManager) setMergeProgress(identifier string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil {
		current.ProgressSec = seconds
		m.persistTaskLocked(current)
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
	current := m.tasks[identifier]
	if current == nil || current.Status == statusCancelled {
		m.mu.Unlock()
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
	m.persistTaskLocked(current)
	m.persistLogLocked(current)
	handler := m.onCompleted
	copy := *current
	copy.Logs = append([]taskLog(nil), current.Logs...)
	m.mu.Unlock()
	if status == statusCompleted && handler != nil {
		handler(&copy)
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
	m.persistTaskLocked(current)
	m.persistLogLocked(current)
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
		m.persistTaskLocked(current)
		m.persistLogLocked(current)
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
		m.persistTaskLocked(current)
		m.persistLogLocked(current)
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
		m.persistTaskLocked(current)
		m.persistLogLocked(current)
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
		m.persistTaskLocked(current)
		m.persistLogLocked(current)
		return true
	}
	return false
}

func (m *taskManager) persistTaskLocked(current *task) {
	if m.store == nil {
		return
	}
	if err := m.store.saveTask(current); err != nil {
		log.Printf("保存任务 %s 失败: %v", current.ID, err)
	}
}

func (m *taskManager) persistLogLocked(current *task) {
	if m.store == nil || len(current.Logs) == 0 {
		return
	}
	if err := m.store.saveTaskLog(current.ID, current.LogSequence, current.Logs[len(current.Logs)-1]); err != nil {
		log.Printf("保存任务日志 %s 失败: %v", current.ID, err)
	}
}
