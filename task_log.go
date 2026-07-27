package main

import (
	"strings"
	"time"
)

const maxTaskLogEntries = 240

type taskLog struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

func (m *taskManager) addLog(identifier, level, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.tasks[identifier]; current != nil {
		appendTaskLog(current, level, message)
	}
}

func appendTaskLog(current *task, level, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	current.Logs = append(current.Logs, taskLog{
		At:      time.Now(),
		Level:   level,
		Message: message,
	})
	if len(current.Logs) > maxTaskLogEntries {
		current.Logs = append([]taskLog(nil), current.Logs[len(current.Logs)-maxTaskLogEntries:]...)
	}
}
