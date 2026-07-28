//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
)

func selectDirectory(initialDirectory string) (string, error) {
	const script = `$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.Description = '选择目录'
$dialog.SelectedPath = [Environment]::GetEnvironmentVariable('M3U8_INITIAL_DIRECTORY')
if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $dialog.SelectedPath }`

	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", "Add-Type -AssemblyName System.Windows.Forms; "+script)
	command.Env = append(os.Environ(), "M3U8_INITIAL_DIRECTORY="+initialDirectory)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func selectFFmpegExecutable(initialPath string) (string, error) {
	const script = `$dialog = New-Object System.Windows.Forms.OpenFileDialog
$dialog.Title = '选择 FFmpeg 可执行文件'
$dialog.Filter = 'FFmpeg (ffmpeg.exe)|ffmpeg.exe|可执行文件 (*.exe)|*.exe'
$initialPath = [Environment]::GetEnvironmentVariable('M3U8_INITIAL_FILE')
if (Test-Path -LiteralPath $initialPath -PathType Leaf) { $dialog.FileName = $initialPath }
elseif (Test-Path -LiteralPath $initialPath -PathType Container) { $dialog.InitialDirectory = $initialPath }
if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $dialog.FileName }`

	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", "Add-Type -AssemblyName System.Windows.Forms; "+script)
	command.Env = append(os.Environ(), "M3U8_INITIAL_FILE="+initialPath)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
