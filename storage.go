package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const databaseFileName = "m3u8-downloader.db"

type appSettings struct {
	OutputDir           string `json:"outputDirectory"`
	CacheDir            string `json:"cacheDirectory"`
	DeleteCache         bool   `json:"deleteCache"`
	WorkerCount         int    `json:"workerCount"`
	CacheRetentionHours int    `json:"cacheRetentionHours"`
}

type telegramSettings struct {
	APIBaseURL  string
	BotToken    string
	ChatIDs     []int64
	AutoUpload  bool
	SplitSizeMB int
}

type store struct {
	db *sql.DB
}

func openStore(path, initialPassword string) (*store, string, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, "", fmt.Errorf("打开 SQLite 数据库失败: %w", err)
	}
	database.SetMaxOpenConns(1)
	result := &store{db: database}
	if err := result.migrate(); err != nil {
		_ = database.Close()
		return nil, "", err
	}
	password, err := result.ensureAdmin(initialPassword)
	if err != nil {
		_ = database.Close()
		return nil, "", err
	}
	return result, password, nil
}

func (s *store) close() error {
	return s.db.Close()
}

func (s *store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
  username TEXT PRIMARY KEY,
  password_hash BLOB NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  source_url TEXT NOT NULL,
  referer TEXT NOT NULL,
  cookie TEXT NOT NULL,
  user_agent TEXT NOT NULL,
  mode TEXT NOT NULL,
  phase TEXT NOT NULL,
  output_name TEXT NOT NULL,
  output_dir TEXT NOT NULL,
  cache_dir TEXT NOT NULL,
  delete_cache INTEGER NOT NULL,
  worker_count INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  output_path TEXT NOT NULL,
  status TEXT NOT NULL,
  completed_segments INTEGER NOT NULL,
  total_segments INTEGER NOT NULL,
  downloaded_bytes INTEGER NOT NULL,
  progress_sec REAL NOT NULL,
  duration_sec REAL NOT NULL,
  created_at INTEGER NOT NULL,
  started_at INTEGER,
  finished_at INTEGER,
  error TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_logs (
  task_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  at INTEGER NOT NULL,
  level TEXT NOT NULL,
  message TEXT NOT NULL,
  PRIMARY KEY (task_id, sequence),
  FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS task_logs_task_sequence ON task_logs(task_id, sequence);
`)
	if err != nil {
		return fmt.Errorf("初始化 SQLite 数据表失败: %w", err)
	}
	return nil
}

func defaultSettings() (appSettings, error) {
	outputDir, err := filepath.Abs(downloadDir)
	if err != nil {
		return appSettings{}, err
	}
	cacheDir, err := filepath.Abs(defaultCacheDir)
	if err != nil {
		return appSettings{}, err
	}
	return appSettings{OutputDir: outputDir, CacheDir: cacheDir, DeleteCache: true, WorkerCount: 8, CacheRetentionHours: 168}, nil
}

func (s *store) loadSettings(fallback appSettings) (appSettings, error) {
	defaults := map[string]string{
		"output_directory":      fallback.OutputDir,
		"cache_directory":       fallback.CacheDir,
		"delete_cache":          strconv.FormatBool(fallback.DeleteCache),
		"worker_count":          strconv.Itoa(fallback.WorkerCount),
		"cache_retention_hours": strconv.Itoa(fallback.CacheRetentionHours),
	}
	for key, value := range defaults {
		if _, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, key, value); err != nil {
			return appSettings{}, fmt.Errorf("保存默认设置失败: %w", err)
		}
	}
	values := make(map[string]string, len(defaults))
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return appSettings{}, fmt.Errorf("读取默认设置失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return appSettings{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return appSettings{}, err
	}
	deleteCache, err := strconv.ParseBool(values["delete_cache"])
	if err != nil {
		return appSettings{}, errors.New("默认缓存设置无效")
	}
	workerCount, err := strconv.Atoi(values["worker_count"])
	if err != nil || validateWorkerCount(workerCount) != nil {
		return appSettings{}, errors.New("默认并发数设置无效")
	}
	cacheRetentionHours, err := strconv.Atoi(values["cache_retention_hours"])
	if err != nil || validateCacheRetentionHours(cacheRetentionHours) != nil {
		return appSettings{}, errors.New("缓存保留时长设置无效")
	}
	return appSettings{OutputDir: values["output_directory"], CacheDir: values["cache_directory"], DeleteCache: deleteCache, WorkerCount: workerCount, CacheRetentionHours: cacheRetentionHours}, nil
}

func (s *store) saveSettings(settings appSettings) error {
	values := map[string]string{
		"output_directory":      settings.OutputDir,
		"cache_directory":       settings.CacheDir,
		"delete_cache":          strconv.FormatBool(settings.DeleteCache),
		"worker_count":          strconv.Itoa(settings.WorkerCount),
		"cache_retention_hours": strconv.Itoa(settings.CacheRetentionHours),
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for key, value := range values {
		if _, err := transaction.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func defaultTelegramSettings() telegramSettings {
	return telegramSettings{APIBaseURL: "http://127.0.0.1:8081", SplitSizeMB: 1900}
}

func (s *store) loadTelegramSettings() (telegramSettings, error) {
	defaults := defaultTelegramSettings()
	values := map[string]string{
		"telegram_api_base_url":  defaults.APIBaseURL,
		"telegram_bot_token":     "",
		"telegram_chat_ids":      "",
		"telegram_auto_upload":   "false",
		"telegram_split_size_mb": strconv.Itoa(defaults.SplitSizeMB),
	}
	for key, value := range values {
		if _, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, key, value); err != nil {
			return telegramSettings{}, err
		}
	}
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE key LIKE 'telegram_%'`)
	if err != nil {
		return telegramSettings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return telegramSettings{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return telegramSettings{}, err
	}
	chatIDs, err := parseTelegramChatIDs(values["telegram_chat_ids"])
	if err != nil {
		return telegramSettings{}, err
	}
	autoUpload, err := strconv.ParseBool(values["telegram_auto_upload"])
	if err != nil {
		return telegramSettings{}, errors.New("Telegram 自动上传设置无效")
	}
	splitSize, err := strconv.Atoi(values["telegram_split_size_mb"])
	if err != nil || splitSize < 1 || splitSize > 2000 {
		return telegramSettings{}, errors.New("Telegram 切分大小必须为 1 至 2000 MB")
	}
	return telegramSettings{APIBaseURL: strings.TrimRight(strings.TrimSpace(values["telegram_api_base_url"]), "/"), BotToken: strings.TrimSpace(values["telegram_bot_token"]), ChatIDs: chatIDs, AutoUpload: autoUpload, SplitSizeMB: splitSize}, nil
}

func (s *store) saveTelegramSettings(settings telegramSettings) error {
	values := map[string]string{
		"telegram_api_base_url":  settings.APIBaseURL,
		"telegram_bot_token":     settings.BotToken,
		"telegram_chat_ids":      telegramChatIDsText(settings.ChatIDs),
		"telegram_auto_upload":   strconv.FormatBool(settings.AutoUpload),
		"telegram_split_size_mb": strconv.Itoa(settings.SplitSizeMB),
	}
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for key, value := range values {
		if _, err := transaction.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func parseTelegramChatIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.FieldsFunc(raw, func(character rune) bool {
		return character == ',' || character == '\n' || character == '\r' || character == ' ' || character == '\t'
	})
	seen := make(map[int64]struct{}, len(parts))
	chatIDs := make([]int64, 0, len(parts))
	for _, part := range parts {
		identifier, err := strconv.ParseInt(part, 10, 64)
		if err != nil || identifier == 0 {
			return nil, errors.New("Telegram Chat ID 格式无效")
		}
		if _, exists := seen[identifier]; !exists {
			seen[identifier] = struct{}{}
			chatIDs = append(chatIDs, identifier)
		}
	}
	return chatIDs, nil
}

func telegramChatIDsText(chatIDs []int64) string {
	items := make([]string, 0, len(chatIDs))
	for _, identifier := range chatIDs {
		items = append(items, strconv.FormatInt(identifier, 10))
	}
	return strings.Join(items, ",")
}

func (s *store) saveTask(current *task) error {
	_, err := s.db.Exec(`
INSERT INTO tasks(id, source_url, referer, cookie, user_agent, mode, phase, output_name, output_dir, cache_dir, delete_cache, worker_count, cache_key, output_path, status, completed_segments, total_segments, downloaded_bytes, progress_sec, duration_sec, created_at, started_at, finished_at, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 source_url=excluded.source_url, referer=excluded.referer, cookie=excluded.cookie, user_agent=excluded.user_agent, mode=excluded.mode, phase=excluded.phase, output_name=excluded.output_name, output_dir=excluded.output_dir, cache_dir=excluded.cache_dir, delete_cache=excluded.delete_cache, worker_count=excluded.worker_count, cache_key=excluded.cache_key, output_path=excluded.output_path, status=excluded.status, completed_segments=excluded.completed_segments, total_segments=excluded.total_segments, downloaded_bytes=excluded.downloaded_bytes, progress_sec=excluded.progress_sec, duration_sec=excluded.duration_sec, created_at=excluded.created_at, started_at=excluded.started_at, finished_at=excluded.finished_at, error=excluded.error`,
		current.ID, current.SourceURL, current.Referer, current.Cookie, current.UserAgent, current.Mode, current.Phase, current.OutputName, current.OutputDir, current.CacheDir, current.DeleteCache, current.WorkerCount, current.CacheKey, current.OutputPath, current.Status, current.CompletedSegments, current.TotalSegments, current.DownloadedBytes, current.ProgressSec, current.DurationSec, current.CreatedAt.UnixMilli(), nullableTime(current.StartedAt), nullableTime(current.FinishedAt), current.Error)
	return err
}

func (s *store) saveTaskLog(identifier string, sequence int, entry taskLog) error {
	transaction, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec(`INSERT INTO task_logs(task_id, sequence, at, level, message) VALUES (?, ?, ?, ?, ?)`, identifier, sequence, entry.At.UnixMilli(), entry.Level, entry.Message); err != nil {
		return err
	}
	if _, err := transaction.Exec(`DELETE FROM task_logs WHERE task_id = ? AND sequence <= (SELECT COALESCE(MAX(sequence), 0) - ? FROM task_logs WHERE task_id = ?)`, identifier, maxTaskLogEntries, identifier); err != nil {
		return err
	}
	return transaction.Commit()
}

func (s *store) loadTasks() ([]*task, error) {
	rows, err := s.db.Query(`SELECT id, source_url, referer, cookie, user_agent, mode, phase, output_name, output_dir, cache_dir, delete_cache, worker_count, cache_key, output_path, status, completed_segments, total_segments, downloaded_bytes, progress_sec, duration_sec, created_at, started_at, finished_at, error FROM tasks ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	items := make([]*task, 0)
	for rows.Next() {
		current := &task{}
		var status string
		var createdAt int64
		var startedAt, finishedAt sql.NullInt64
		if err := rows.Scan(&current.ID, &current.SourceURL, &current.Referer, &current.Cookie, &current.UserAgent, &current.Mode, &current.Phase, &current.OutputName, &current.OutputDir, &current.CacheDir, &current.DeleteCache, &current.WorkerCount, &current.CacheKey, &current.OutputPath, &status, &current.CompletedSegments, &current.TotalSegments, &current.DownloadedBytes, &current.ProgressSec, &current.DurationSec, &createdAt, &startedAt, &finishedAt, &current.Error); err != nil {
			return nil, err
		}
		current.Status = taskStatus(status)
		current.CreatedAt = time.UnixMilli(createdAt)
		current.StartedAt = timeFromNull(startedAt)
		current.FinishedAt = timeFromNull(finishedAt)
		items = append(items, current)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, current := range items {
		logs, sequence, err := s.loadTaskLogs(current.ID)
		if err != nil {
			return nil, err
		}
		current.Logs, current.LogSequence = logs, sequence
	}
	return items, nil
}

func (s *store) loadTaskLogs(identifier string) ([]taskLog, int, error) {
	rows, err := s.db.Query(`SELECT sequence, at, level, message FROM task_logs WHERE task_id = ? ORDER BY sequence`, identifier)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	logs := make([]taskLog, 0)
	sequence := 0
	for rows.Next() {
		var entry taskLog
		var at int64
		if err := rows.Scan(&sequence, &at, &entry.Level, &entry.Message); err != nil {
			return nil, 0, err
		}
		entry.At = time.UnixMilli(at)
		logs = append(logs, entry)
	}
	return logs, sequence, rows.Err()
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}

func timeFromNull(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.UnixMilli(value.Int64)
	return &result
}
