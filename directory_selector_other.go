//go:build !windows

package main

import "errors"

func selectDirectory(initialDirectory string) (string, error) {
	return "", errors.New("当前平台不支持系统目录选择")
}
