//go:build darwin

package darwin

import (
	"os/exec"

	"github.com/wailsapp/wails/v2/internal/conv"
)

func (f *Frontend) ClipboardGetText() (string, error) {
	pasteCmd := exec.Command("pbpaste")
	out, err := pasteCmd.Output()
	if err != nil {
		return "", err
	}
	return conv.BytesToString(out), nil
}

func (f *Frontend) ClipboardSetText(text string) error {
	copyCmd := exec.Command("pbcopy")
	in, err := copyCmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := copyCmd.Start(); err != nil {
		return err
	}
	if _, err := in.Write([]byte(text)); err != nil {
		return err
	}
	if err := in.Close(); err != nil {
		return err
	}
	return copyCmd.Wait()
}
