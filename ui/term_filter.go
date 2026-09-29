package ui

import "unicode/utf8"

// outputFilter sanitizes terminal output from the server before it reaches
// the terminal widget. It is a streaming parser for ECMA-48 control
// sequences: state carries over between writes, and only what is held back
// for an unfinished sequence is buffered (a bounded amount).
//
// It passes plain text, the usual C0 controls, ESC sequences and CSI
// sequences the widget understands, and window titles (OSC 0/1/2). It
// drops what the widget does not support or mishandles:
//   - media copy (CSI … i): the widget buffers everything after ESC[5i
//     until ESC[4i, and there is no printer anyway;
//   - other OSC commands, e.g. OSC 7, which made the widget os.Chdir the
//     client process, OSC 52 (clipboard) and color queries;
//   - DCS, APC, PM and SOS strings;
//   - CSI sequences with intermediate bytes or ':'/'<' parameters (the
//     widget prints part of them as text) and overlong ones.
//
// Numeric CSI parameters are capped, so no sequence can make the widget
// loop for minutes.
type outputFilter struct {
	state filterState
	held  []byte // sequence collected so far (ESC …, CSI … or OSC text)
	skip  bool   // current CSI sequence is dropped at its final byte
	n     int    // length of a string being discarded
}

type filterState uint8

const (
	fGround    filterState = iota
	fEsc                   // after ESC
	fEscInter              // ESC + intermediate bytes, e.g. "ESC ("
	fCSI                   // after ESC [
	fOSC                   // after ESC ]
	fOSCEsc                // ESC inside an OSC string: ST or a new sequence
	fString                // DCS/APC/PM/SOS or unwanted OSC: discarded
	fStringEsc             // ESC inside a discarded string
)

const (
	esc = 0x1b
	bel = 0x07
	can = 0x18
	sub = 0x1a

	maxCSILen    = 64      // longer CSI sequences are dropped
	maxEscLen    = 8       // ESC + intermediates
	maxTitleLen  = 256     // OSC 0/1/2 text
	maxStringLen = 1 << 16 // a discarded string ends after this many bytes
	maxParam     = "9999"  // cap for numeric CSI parameters
)

// write appends the sanitized form of p to dst.
func (f *outputFilter) write(dst, p []byte) []byte {
	for _, c := range p {
		dst = f.step(dst, c)
	}
	return dst
}

func (f *outputFilter) step(dst []byte, c byte) []byte {
	switch f.state {
	case fGround:
		return f.ground(dst, c)

	case fEsc:
		switch {
		case c == '[':
			f.held = append(f.held[:0], esc, '[')
			f.skip = false
			f.state = fCSI
		case c == ']':
			f.held = f.held[:0]
			f.state = fOSC
		case c == 'P' || c == 'X' || c == '^' || c == '_': // DCS, SOS, PM, APC
			f.n = 0
			f.state = fString
		case c >= 0x20 && c <= 0x2f: // intermediate, e.g. ESC ( B
			f.held = append(f.held[:0], esc, c)
			f.state = fEscInter
		case c >= 0x30 && c <= 0x7e: // ESC 7, ESC 8, ESC M, ESC =, …
			dst = append(dst, esc, c)
			f.state = fGround
		default: // control, DEL or non-ASCII byte: not a sequence
			f.state = fGround
			return f.ground(dst, c)
		}

	case fEscInter:
		switch {
		case c >= 0x20 && c <= 0x2f && len(f.held) < maxEscLen:
			f.held = append(f.held, c)
		case c >= 0x30 && c <= 0x7e:
			dst = append(append(dst, f.held...), c)
			f.state = fGround
		default:
			f.state = fGround
			return f.ground(dst, c)
		}

	case fCSI:
		switch {
		case c >= 0x30 && c <= 0x3f: // parameter byte
			if c == ':' || c == '<' {
				f.skip = true
			}
			f.addCSI(c)
		case c >= 0x20 && c <= 0x2f: // intermediate byte
			f.skip = true
			f.addCSI(c)
		case c >= 0x40 && c <= 0x7e: // final byte
			f.state = fGround
			if !f.skip && c != 'i' {
				dst = appendCSI(dst, f.held, c)
			}
		default:
			f.state = fGround
			return f.ground(dst, c)
		}

	case fOSC:
		switch {
		case c == bel || c == 0:
			dst = appendOSC(dst, f.held)
			f.state = fGround
		case c == esc:
			f.state = fOSCEsc
		case c == can || c == sub:
			f.state = fGround
		case c < 0x20 || c == 0x7f || len(f.held) >= maxTitleLen+2:
			// Not a title we pass on: drop the rest of it.
			f.n = 0
			f.state = fString
		default:
			f.held = append(f.held, c)
		}

	case fOSCEsc:
		if c == '\\' { // ST
			dst = appendOSC(dst, f.held)
			f.state = fGround
			return dst
		}
		// The ESC cancels the string and starts a new sequence.
		f.state = fEsc
		return f.step(dst, c)

	case fString:
		switch c {
		case esc:
			f.state = fStringEsc
		case bel, can, sub:
			f.state = fGround
		default:
			if f.n++; f.n > maxStringLen {
				f.state = fGround
			}
		}

	case fStringEsc:
		if c == '\\' { // ST
			f.state = fGround
			return dst
		}
		f.state = fEsc
		return f.step(dst, c)
	}
	return dst
}

// ground handles a byte outside of any sequence.
func (f *outputFilter) ground(dst []byte, c byte) []byte {
	switch {
	case c == esc:
		f.state = fEsc
	case c < 0x20:
		// BEL, BS, HT, LF, VT, FF, CR, SO, SI; the widget would print
		// other C0 controls as characters.
		if c >= bel && c <= 0x0f {
			dst = append(dst, c)
		}
	case c == 0x7f: // DEL
	default:
		dst = append(dst, c)
	}
	return dst
}

func (f *outputFilter) addCSI(c byte) {
	if len(f.held) >= maxCSILen {
		f.skip = true // overlong: dropped, but consumed up to its final byte
		return
	}
	f.held = append(f.held, c)
}

// appendCSI appends the CSI sequence held + final to dst with numeric
// parameters capped: leading zeros are removed and numbers above maxParam
// are replaced by it.
func appendCSI(dst, held []byte, final byte) []byte {
	dst = append(dst, held[:2]...) // ESC [
	params := held[2:]
	for i := 0; i < len(params); {
		if params[i] < '0' || params[i] > '9' {
			dst = append(dst, params[i])
			i++
			continue
		}
		j := i
		for j < len(params) && params[j] >= '0' && params[j] <= '9' {
			j++
		}
		num := params[i:j]
		for len(num) > 1 && num[0] == '0' {
			num = num[1:]
		}
		if len(num) > len(maxParam) || (len(num) == len(maxParam) && string(num) > maxParam) {
			num = []byte(maxParam)
		}
		dst = append(dst, num...)
		i = j
	}
	return append(dst, final)
}

// appendOSC appends the OSC command text to dst if it sets the window or
// icon title (OSC 0, 1 or 2) to printable, valid UTF-8 text.
func appendOSC(dst, text []byte) []byte {
	if len(text) < 2 || text[1] != ';' || (text[0] != '0' && text[0] != '1' && text[0] != '2') {
		return dst
	}
	title := text[2:]
	if len(title) > maxTitleLen || !utf8.Valid(title) {
		return dst
	}
	for _, r := range string(title) {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return dst
		}
	}
	dst = append(dst, esc, ']')
	dst = append(dst, text...)
	return append(dst, bel)
}
