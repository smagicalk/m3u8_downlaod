package main

import "time"

type taskStatus string

const (
	statusQueued    taskStatus = "queued"
	statusRunning   taskStatus = "running"
	statusCompleted taskStatus = "completed"
	statusFailed    taskStatus = "failed"
	statusCancelled taskStatus = "cancelled"
	statusPaused    taskStatus = "paused"
)

const (
	modeStream        = "stream"
	modeDownloadFirst = "download-first"
)

type task struct {
	ID                string     `json:"id"`
	SourceURL         string     `json:"sourceUrl"`
	Referer           string     `json:"-"`
	Cookie            string     `json:"-"`
	UserAgent         string     `json:"-"`
	Mode              string     `json:"mode"`
	Phase             string     `json:"phase,omitempty"`
	OutputName        string     `json:"outputName"`
	OutputDir         string     `json:"outputDirectory"`
	CacheDir          string     `json:"cacheDirectory,omitempty"`
	DeleteCache       bool       `json:"deleteCache"`
	WorkerCount       int        `json:"workerCount"`
	CacheKey          string     `json:"-"`
	OutputPath        string     `json:"-"`
	Status            taskStatus `json:"status"`
	CompletedSegments int        `json:"completedSegments"`
	TotalSegments     int        `json:"totalSegments"`
	DownloadedBytes   int64      `json:"downloadedBytes"`
	ProgressSec       float64    `json:"progressSec"`
	DurationSec       float64    `json:"durationSec"`
	Logs              []taskLog  `json:"logs"`
	LogSequence       int        `json:"-"`
	CreatedAt         time.Time  `json:"createdAt"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
	Error             string     `json:"error,omitempty"`
}

type createRequest struct {
	SourceURL           string `json:"sourceUrl"`
	Referer             string `json:"referer"`
	Cookie              string `json:"cookie"`
	UserAgent           string `json:"userAgent"`
	Mode                string `json:"mode"`
	OutputDir           string `json:"outputDirectory"`
	CacheDir            string `json:"cacheDirectory"`
	DeleteCache         *bool  `json:"deleteCache"`
	ConcurrentDownloads *bool  `json:"concurrentDownloads"`
	WorkerCount         *int   `json:"workerCount"`
	OutputName          string `json:"outputName"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

type directorySelectionRequest struct {
	InitialDirectory string `json:"initialDirectory"`
}

type directorySelectionResponse struct {
	Path string `json:"path"`
}

type fileSelectionRequest struct {
	InitialPath string `json:"initialPath"`
}
