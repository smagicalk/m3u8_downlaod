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
	ID          string     `json:"id"`
	SourceURL   string     `json:"sourceUrl"`
	Referer     string     `json:"-"`
	Cookie      string     `json:"-"`
	UserAgent   string     `json:"-"`
	Mode        string     `json:"mode"`
	Phase       string     `json:"phase,omitempty"`
	OutputName  string     `json:"outputName"`
	OutputDir   string     `json:"outputDirectory"`
	CacheDir    string     `json:"cacheDirectory,omitempty"`
	DeleteCache bool       `json:"deleteCache"`
	OutputPath  string     `json:"-"`
	Status      taskStatus `json:"status"`
	ProgressSec float64    `json:"progressSec"`
	DurationSec float64    `json:"durationSec"`
	CreatedAt   time.Time  `json:"createdAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type createRequest struct {
	SourceURL   string `json:"sourceUrl"`
	Referer     string `json:"referer"`
	Cookie      string `json:"cookie"`
	UserAgent   string `json:"userAgent"`
	Mode        string `json:"mode"`
	OutputDir   string `json:"outputDirectory"`
	CacheDir    string `json:"cacheDirectory"`
	DeleteCache bool   `json:"deleteCache"`
	OutputName  string `json:"outputName"`
}

type directorySelectionRequest struct {
	InitialDirectory string `json:"initialDirectory"`
}

type directorySelectionResponse struct {
	Path string `json:"path"`
}
