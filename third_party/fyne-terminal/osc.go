package terminal

import (
	"log"
)

func (t *Terminal) handleOSC(code string) {
	if len(code) <= 2 || code[1] != ';' {
		return
	}

	switch code[0] {
	case '0':
		// set icon name, if Fyne supports in the future
		t.setTitle(code[2:])
	case '1':
		// set icon name, if Fyne supports in the future
	case '2':
		t.setTitle(code[2:])
	case '7':
		// MTSSH patch: OSC 7 comes from the remote side and used to
		// os.Chdir the whole client process (and could panic on short
		// URIs). It is ignored.
	default:
		if t.debug {
			log.Println("Unrecognised OSC:", code)
		}
	}
}

func (t *Terminal) setTitle(title string) {
	t.config.Title = title
	t.onConfigure()
}
