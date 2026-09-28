package vt

// PrimaryScreen returns the stable primary buffer object.
func (e *Emulator) PrimaryScreen() *Screen { return &e.scrs[0] }

// AlternateScreen returns the stable alternate buffer object.
func (e *Emulator) AlternateScreen() *Screen { return &e.scrs[1] }

// PendingWrap reports deferred autowrap continuation.
func (e *Emulator) PendingWrap() bool { return e.atPhantom }

// SavedCursor returns the full DECSC save point.
func (s *Screen) SavedCursor() Cursor { return s.saved }
