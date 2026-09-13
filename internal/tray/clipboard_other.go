//go:build !windows

package tray

// Wayland first, then X11, then macOS: whichever is present wins.
func clipboardCommands() [][]string {
	return [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"pbcopy"},
	}
}
