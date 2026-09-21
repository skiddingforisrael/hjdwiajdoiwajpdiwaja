package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

var errSelectionCanceled = errors.New("file selection canceled")

// SelectTexturePackZIP opens the operating system's file selector and returns
// the selected Minecraft texture-pack ZIP.
func SelectTexturePackZIP() (string, error) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		if path, err := exec.LookPath("zenity"); err == nil {
			command = exec.Command(path,
				"--file-selection",
				"--title=Select Minecraft 1.8.9 Texture Pack",
				"--file-filter=Texture packs | *.zip *.ZIP",
			)
		} else if path, err := exec.LookPath("kdialog"); err == nil {
			command = exec.Command(path, "--getopenfilename", "", "*.zip|Minecraft texture packs")
		}
	case "darwin":
		command = exec.Command("osascript", "-e",
			`POSIX path of (choose file with prompt "Select Minecraft 1.8.9 Texture Pack" of type {"zip"})`)
	case "windows":
		script := `Add-Type -AssemblyName System.Windows.Forms; ` +
			`$d=New-Object System.Windows.Forms.OpenFileDialog; ` +
			`$d.Title='Select Minecraft 1.8.9 Texture Pack'; ` +
			`$d.Filter='Texture packs (*.zip)|*.zip'; ` +
			`if($d.ShowDialog() -eq 'OK'){[Console]::Write($d.FileName)}else{exit 1}`
		command = exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script)
	}
	if command == nil {
		return "", errors.New("no supported file selector found (install zenity or kdialog)")
	}
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", errSelectionCanceled
		}
		return "", fmt.Errorf("open texture-pack file selector: %w", err)
	}
	selected := strings.TrimSpace(string(output))
	if selected == "" {
		return "", errSelectionCanceled
	}
	return selected, nil
}
