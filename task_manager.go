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

func (m *taskManager) create(sourceURL, referer, cookie, userAgent, mode, outputName, outputDir, cacheDir string, deleteCache bool) (*task, error) {
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
		ID:          identifier,
		SourceURL:   sourceURL,
		Referer:     strings.TrimSpace(referer),
		Cookie:      strings.TrimSpace(cookie),
		UserAgent:   normalizeUserAgent(userAgent),
		Mode:        normalizeMode(mode),
		OutputName:  name,
		OutputDir:   resolvedOutputDir,
		CacheDir:    resolvedCacheDir,
		DeleteCache: deleteCache,
		OutputPath:  filepath.Join(resolvedOutputDir, name),
		Status:      statusQueued,
		CreatedAt:   time.Now(),
	}

	m.mu.Lock()
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
	m.mu.RUnlock()

	inputURL := m.playlistProxyURL(identifier)
	if inputURL == "" {
		m.finish(identifier, statusFailed, "本地 HLS 代理未初始化")
		return
	}
	m.setPhase(identifier, phaseForMode(mode))
	onProgress := func(seconds float64) {
		m.mu.Lock()
		if current := m.tasks[identifier]; current != nil {
			current.ProgressSec = seconds
		}
		m.mu.Unlock()
	}
	onProcess := func(process *os.Process) { m.setProcess(identifier, process) }
	defer m.setProcess(identifier, nil)
	if mode == modeDownloadFirst {
		temporaryPath := filepath.Join(cacheDir, "."+identifier+".ts")
		err := runFFmpeg(ctx, inputURL, temporaryPath, "mpegts", onProgress, onProcess)
		if err == nil {
			m.setPhase(identifier, "merging")
			err = runFFmpeg(ctx, temporaryPath, outputPath, "mp4", onProgress, onProcess)
		}
		if err == nil && deleteCache {
			_ = os.Remove(temporaryPath)
		}
		if err != nil {
			m.finish(identifier, statusFailed, err.Error())
			return
		}
	} else if err := runFFmpeg(ctx, inputURL, outputPath, "mp4", onProgress, onProcess); err != nil {
		m.finish(identifier, statusFailed, err.Error())
		return
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
	return &copy
}

func (m *taskManager) all() []*task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]*task, 0, len(m.tasks))
	for _, current := range m.tasks {
		copy := *current
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
		return true
	}
	return false
}

var errTaskNotFound = errors.New("task not found")
