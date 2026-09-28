package vt

import (
	"bytes"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestCheckpointPreservesReachableRawStrings(t *testing.T) {
	invalid := append([]byte("https://example.invalid/"), 0xff)
	for _, tc := range []struct {
		name, prefix, suffix string
		value                []byte
		read                 func(*Emulator) string
	}{
		{"osc8_params", "\x1b]8;;", "\x1b\\", invalid, func(e *Emulator) string { return e.scr.cur.Link.Params }},
		{"osc8_url", "\x1b]8;", ";id=x\x1b\\", invalid, func(e *Emulator) string { return e.scr.cur.Link.URL }},
		{"osc7_cwd", "\x1b]7;", "\x1b\\", invalid, func(e *Emulator) string { return e.cwd }},
		{"osc1_icon", "\x1b]1;", "\x1b\\", invalid, func(e *Emulator) string { return e.iconName }},
		{"osc2_title", "\x1b]2;", "\x1b\\", invalid, func(e *Emulator) string { return e.title }},
		{"valid_utf8_link", "\x1b]8;;", "\x1b\\", []byte("https://example.invalid/é"), func(e *Emulator) string { return e.scr.cur.Link.Params }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer := NewEmulator(20, 8)
			t.Cleanup(func() { _ = producer.Close() })
			history := append([]byte(tc.prefix), tc.value...)
			history = append(history, []byte(tc.suffix)...)
			if _, err := producer.Write(history); err != nil {
				t.Fatal(err)
			}
			before := []byte(tc.read(producer))
			if !bytes.Equal(before, tc.value) {
				t.Fatalf("fixture did not reach raw field: got=%x want=%x", before, tc.value)
			}
			image, err := producer.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			mirror := NewEmulator(20, 8)
			t.Cleanup(func() { _ = mirror.Close() })
			if err := mirror.RestoreCheckpoint(image); err != nil {
				t.Fatalf("reachable raw state failed restore: %v", err)
			}
			if after := []byte(tc.read(mirror)); !bytes.Equal(after, before) {
				t.Fatalf("raw state changed: before=%x after=%x", before, after)
			}
			for _, e := range []*Emulator{producer, mirror} {
				if _, err := e.WriteString("X"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "osc8_params" || tc.name == "osc8_url" || tc.name == "valid_utf8_link" {
				p, m := producer.PrimaryScreen().CellAt(0, 0), mirror.PrimaryScreen().CellAt(0, 0)
				if p == nil || m == nil || !bytes.Equal([]byte(p.Link.URL), []byte(m.Link.URL)) || !bytes.Equal([]byte(p.Link.Params), []byte(m.Link.Params)) {
					t.Fatalf("printed cell link diverged: producer=%+v mirror=%+v", p, m)
				}
			}
			if producer.Render() != mirror.Render() {
				t.Fatal("same suffix changed rendered screen")
			}
		})
	}
}

func TestCheckpointPreservesCellContentBytes(t *testing.T) {
	producer := NewEmulator(2, 1)
	t.Cleanup(func() { _ = producer.Close() })
	raw := string([]byte{'X', 0xff})
	producer.PrimaryScreen().SetCell(0, 0, &uv.Cell{Content: raw, Width: 1})
	if got := producer.PrimaryScreen().CellAt(0, 0); got == nil || got.Content != raw {
		t.Fatal("supported cell setter did not retain raw content")
	}
	image, err := producer.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	mirror := NewEmulator(2, 1)
	t.Cleanup(func() { _ = mirror.Close() })
	if err := mirror.RestoreCheckpoint(image); err != nil {
		t.Fatal(err)
	}
	if got := mirror.PrimaryScreen().CellAt(0, 0); got == nil || got.Content != raw {
		t.Fatalf("raw cell content changed: got=%+v want=%x", got, []byte(raw))
	}
}
