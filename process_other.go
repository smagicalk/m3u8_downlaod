//go:build !windows

package main

import (
	"errors"
	"os"
)

func suspendProcess(process *os.Process) error {
	return errors.New("当前平台不支持暂停 FFmpeg 进程")
}

func resumeProcess(process *os.Process) error {
	return nil
}
