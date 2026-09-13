//go:build windows

package tray

func clipboardCommands() [][]string {
	return [][]string{{"clip"}}
}
