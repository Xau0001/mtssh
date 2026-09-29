package ui

import "testing"

func TestRenderTerminal(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain\r\ntext\n", "plain\ntext\n"},
		// bash prompt with window title (OSC) and bracketed paste mode (CSI)
		{
			"\x1b[?2004h\x1b]0;root@vm: ~\x07root@vm:~# echo hi\r\n\x1b[?2004l\rhi\r\n",
			"root@vm:~# echo hi\nhi\n",
		},
		{"\x1b[01;34mdir\x1b[0m file\n", "dir file\n"},
		{"\x1b]0;title\x1b\\ok", "ok"},
		{"\x1b(Bcharset", "charset"},
		{"10%\r20%\r30%", "30%"},
		{"bell\a and \bback", "bell and back"},
		{"umlaut äöü ✓\n", "umlaut äöü ✓\n"},
		// sequence split across chunks: hidden until the rest arrives
		{"text\x1b[01;3", "text"},
		{"text\x1b", "text"},
	}
	for _, tt := range tests {
		if got := renderTerminal(tt.in); got != tt.want {
			t.Errorf("renderTerminal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
