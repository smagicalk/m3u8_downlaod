//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const processSuspendResume = 0x0800

var (
	ntdll            = syscall.NewLazyDLL("ntdll.dll")
	ntSuspendProcess = ntdll.NewProc("NtSuspendProcess")
	ntResumeProcess  = ntdll.NewProc("NtResumeProcess")
)

func suspendProcess(process *os.Process) error {
	if process == nil {
		return errors.New("FFmpeg 进程不存在")
	}
	handle, err := syscall.OpenProcess(processSuspendResume, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)
	status, _, callErr := ntSuspendProcess.Call(uintptr(handle))
	if status != 0 {
		return fmt.Errorf("暂停 FFmpeg 失败: %v", callErr)
	}
	return nil
}

func resumeProcess(process *os.Process) error {
	if process == nil {
		return nil
	}
	handle, err := syscall.OpenProcess(processSuspendResume, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)
	status, _, callErr := ntResumeProcess.Call(uintptr(handle))
	if status != 0 {
		return fmt.Errorf("继续 FFmpeg 失败: %v", callErr)
	}
	return nil
}
