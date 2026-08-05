package ptyhost

// oscTitleFilter is a narrow streaming filter for OSC 0/1/2 title controls.
// It protects the headless snapshot emulator from title payloads while leaving
// every other terminal sequence available to the emulator, including queries
// it must answer locally. One filter instance is owned by one vtHost.
type oscTitleFilter struct {
	state   oscTitleState
	pending []byte
	utf8Rem int
	oscEsc  bool
}

type oscTitleState uint8

const (
	oscTitleGround oscTitleState = iota
	oscTitleEsc
	oscTitleCommand
	oscTitlePass
	oscTitleDrop
)

const (
	oscTitleBEL   = byte(0x07)
	oscTitleESC   = byte(0x1b)
	oscTitleCAN   = byte(0x18)
	oscTitleSUB   = byte(0x1a)
	oscTitleC1ST  = byte(0x9c)
	oscTitleC1OSC = byte(0x9d)

	// OSC selectors are normally one to four digits. Bounding the undecided
	// prefix prevents malformed input from growing memory without limit.
	oscTitleSelectorMax = 16
)

// Write returns bytes suitable for the snapshot emulator. The returned slice
// never aliases p or internal storage. Partial ESC/OSC state carries across
// calls so every split point is equivalent to a contiguous write.
func (f *oscTitleFilter) Write(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		if f.step(p[i], &out) {
			i++
		}
	}
	return out
}

func (f *oscTitleFilter) step(b byte, out *[]byte) bool {
	switch f.state {
	case oscTitleGround:
		return f.stepGround(b, out)
	case oscTitleEsc:
		if b == ']' {
			f.pending = append(f.pending, b)
			f.state = oscTitleCommand
			return true
		}
		*out = append(*out, f.pending...)
		f.pending = f.pending[:0]
		f.state = oscTitleGround
		return false
	case oscTitleCommand:
		return f.stepCommand(b, out)
	case oscTitlePass:
		return f.stepOSC(b, out, false)
	case oscTitleDrop:
		return f.stepOSC(b, out, true)
	default:
		f.reset()
		return false
	}
}

func (f *oscTitleFilter) stepGround(b byte, out *[]byte) bool {
	if f.utf8Rem > 0 {
		if b >= 0x80 && b <= 0xbf {
			*out = append(*out, b)
			f.utf8Rem--
			return true
		}
		f.utf8Rem = 0
		return false
	}

	switch b {
	case oscTitleESC:
		f.pending = append(f.pending[:0], b)
		f.state = oscTitleEsc
		return true
	case oscTitleC1OSC:
		f.pending = append(f.pending[:0], b)
		f.state = oscTitleCommand
		return true
	default:
		*out = append(*out, b)
		f.utf8Rem = utf8ContinuationCount(b)
		return true
	}
}

func (f *oscTitleFilter) stepCommand(b byte, out *[]byte) bool {
	switch b {
	case ';':
		f.pending = append(f.pending, b)
		if f.isTitleSelector() {
			f.pending = f.pending[:0]
			f.state = oscTitleDrop
			return true
		}
		*out = append(*out, f.pending...)
		f.pending = f.pending[:0]
		f.state = oscTitlePass
		return true
	case oscTitleBEL, oscTitleC1ST:
		if !f.isTitleSelector() {
			*out = append(*out, f.pending...)
			*out = append(*out, b)
		}
		f.reset()
		return true
	case oscTitleESC:
		f.pending = append(f.pending, b)
		f.oscEsc = true
		return true
	case oscTitleCAN, oscTitleSUB:
		// A cancelled OSC has no title effect. Preserve it for the emulator so
		// its parser state remains faithful, then resume ordinary detection.
		*out = append(*out, f.pending...)
		*out = append(*out, b)
		f.reset()
		return true
	}

	if f.oscEsc {
		if b == '\\' {
			if !f.isTitleSelector() {
				*out = append(*out, f.pending...)
				*out = append(*out, b)
			}
			f.reset()
			return true
		}
		// ESC not followed by ST makes this a non-target OSC. Flush the held
		// prefix, then continue passing its payload until a terminator.
		*out = append(*out, f.pending...)
		f.pending = f.pending[:0]
		f.oscEsc = false
		f.state = oscTitlePass
		return false
	}

	if b >= '0' && b <= '9' && len(f.selector()) < oscTitleSelectorMax {
		f.pending = append(f.pending, b)
		return true
	}

	// A command containing anything other than a bounded decimal selector
	// cannot be OSC 0/1/2. Forward it without buffering the remaining payload.
	*out = append(*out, f.pending...)
	*out = append(*out, b)
	f.pending = f.pending[:0]
	f.state = oscTitlePass
	return true
}

func (f *oscTitleFilter) stepOSC(b byte, out *[]byte, drop bool) bool {
	if f.utf8Rem > 0 {
		if b >= 0x80 && b <= 0xbf {
			if !drop {
				*out = append(*out, b)
			}
			f.utf8Rem--
			return true
		}
		f.utf8Rem = 0
		return false
	}

	if f.oscEsc {
		f.oscEsc = false
		if b == '\\' {
			if !drop {
				*out = append(*out, b)
			}
			f.reset()
			return true
		}
		// A non-ST byte remains part of the OSC payload. BEL and C1 ST can
		// still terminate it, so re-dispatch through the ordinary path.
		return false
	}

	switch b {
	case oscTitleESC:
		if !drop {
			*out = append(*out, b)
		}
		f.oscEsc = true
		return true
	case oscTitleBEL, oscTitleC1ST:
		if !drop {
			*out = append(*out, b)
		}
		f.reset()
		return true
	case oscTitleCAN, oscTitleSUB:
		if !drop {
			*out = append(*out, b)
		}
		f.reset()
		return true
	default:
		if !drop {
			*out = append(*out, b)
		}
		f.utf8Rem = utf8ContinuationCount(b)
		return true
	}
}

func (f *oscTitleFilter) selector() []byte {
	intro := 1
	if len(f.pending) >= 2 && f.pending[0] == oscTitleESC && f.pending[1] == ']' {
		intro = 2
	}
	end := len(f.pending)
	if end > intro && (f.pending[end-1] == ';' || f.pending[end-1] == oscTitleESC) {
		end--
	}
	return f.pending[intro:end]
}

func (f *oscTitleFilter) isTitleSelector() bool {
	s := f.selector()
	return len(s) == 1 && (s[0] == '0' || s[0] == '1' || s[0] == '2')
}

func (f *oscTitleFilter) reset() {
	f.state = oscTitleGround
	f.pending = f.pending[:0]
	f.utf8Rem = 0
	f.oscEsc = false
}

func utf8ContinuationCount(b byte) int {
	switch {
	case b >= 0xc2 && b <= 0xdf:
		return 1
	case b >= 0xe0 && b <= 0xef:
		return 2
	case b >= 0xf0 && b <= 0xf4:
		return 3
	default:
		return 0
	}
}
