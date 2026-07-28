package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

func runPasswordReset() error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	storage, _, err := openStore(filepath.Join(dataDir, databaseFileName), os.Getenv("M3U8_ADMIN_PASSWORD"))
	if err != nil {
		return err
	}
	defer storage.close()
	nextPassword, err := readHiddenPassword("请输入新密码：")
	if err != nil {
		return err
	}
	confirmedPassword, err := readHiddenPassword("请再次输入新密码：")
	if err != nil {
		return err
	}
	if nextPassword != confirmedPassword {
		return errors.New("两次输入的密码不一致")
	}
	if err := storage.resetAdminPassword(nextPassword); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "管理员密码已重置，请重新登录。")
	return nil
}

func readHiddenPassword(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("重置密码命令需要在交互式终端中运行")
	}
	fmt.Fprint(os.Stderr, prompt)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("读取密码失败: %w", err)
	}
	return strings.TrimSpace(string(value)), nil
}
