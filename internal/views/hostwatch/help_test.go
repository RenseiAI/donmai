package hostwatch

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func questionKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: '?', Text: "?"} }

// TestModel_HelpOverlayOpensClosesAndPreservesState pins AC-1 through
// production Update/View: "?" opens the overlay in list, detail and
// Unicode-filter contexts; "?" and esc close only the overlay, leaving
// detail, the filter query, cursor, split and follow exactly as they
// were. Routing the help toggle after filter/detail handling (the
// mutant) consumes "?" as query text or detail input instead.
func TestModel_HelpOverlayOpensClosesAndPreservesState(t *testing.T) {
	setup := func(t *testing.T) *Model {
		t.Helper()
		m := newFilterModel(t, 120, 40)
		feedKey(m, keyMsg("down"))
		m.stream.SetFollowing(false)
		m.setSplit(0.7)
		return m
	}
	contexts := map[string]func(*Model){
		"list": func(*Model) {},
		"detail": func(m *Model) {
			feedKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if !m.detail {
				t.Fatal("enter did not open detail")
			}
		},
		"unicode filter": func(m *Model) {
			feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
			feedQuery(m, "日本語")
		},
	}
	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			m := setup(t)
			ctx(m)
			detail, active, query, cursor, split, follow := m.detail, m.filterActive, m.filterQuery, m.cursor, m.split, m.stream.Following()
			visible := visibleIDs(m)

			feedKey(m, questionKey())
			if !m.help {
				t.Fatal("\"?\" did not open the help overlay")
			}
			if m.helpScroll != 0 {
				t.Fatalf("help opened with scroll %d, want 0", m.helpScroll)
			}
			view := m.render()
			for _, want := range []string{"keyboard help", "esc close", "↑↓/jk", "filter", "mouse"} {
				if !strings.Contains(ansi.Strip(view), want) {
					t.Errorf("help overlay missing %q:\n%s", want, ansi.Strip(view))
				}
			}
			// Toggle closed with "?" preserves everything underneath.
			feedKey(m, questionKey())
			if m.help {
				t.Fatal("second \"?\" did not close the help overlay")
			}
			if m.detail != detail || m.filterActive != active || m.filterQuery != query ||
				m.cursor != cursor || m.split != split || m.stream.Following() != follow {
				t.Fatalf("toggle close mutated state: detail %v/%v filter %v/%v query %q/%q cursor %d/%d split %v/%v follow %v/%v",
					m.detail, detail, m.filterActive, active, m.filterQuery, query, m.cursor, cursor, m.split, split, m.stream.Following(), follow)
			}
			if got := visibleIDs(m); !equalIDs(got, visible) {
				t.Fatalf("toggle close changed visible cards %v, want %v", got, visible)
			}
			// Reopen and close with esc: same preservation.
			feedKey(m, questionKey())
			feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.help {
				t.Fatal("esc did not close the help overlay")
			}
			if m.detail != detail || m.filterActive != active || m.filterQuery != query ||
				m.cursor != cursor || m.split != split || m.stream.Following() != follow {
				t.Fatalf("esc close mutated state: detail %v/%v filter %v/%v query %q/%q cursor %d/%d split %v/%v follow %v/%v",
					m.detail, detail, m.filterActive, active, m.filterQuery, query, m.cursor, cursor, m.split, split, m.stream.Following(), follow)
			}
			// Detail still renders and the filter indicator is intact.
			if detail && !strings.Contains(ansi.Strip(m.render()), "session detail") {
				t.Error("detail did not survive help dismissal")
			}
			if active && !strings.Contains(m.render(), "filter: /"+query) {
				t.Errorf("filter indicator lost after help dismissal:\n%s", m.render())
			}
		})
	}
}

// TestModel_HelpOverlayConsumesUnderlyingActions pins AC-2 through
// production Update: while help is open, selection/follow/split/filter/
// lifecycle keys are consumed without mutating underlying state and
// "?" never becomes filter text; ctrl+c still quits; pending
// confirmations keep "?" ignored and armed; resize, snapshot, tail and
// action-result updates still apply; mouse messages are accepted with
// no action. Removing overlay consumption (the mutant) lets "p"
// dispatch a lifecycle request.
func TestModel_HelpOverlayConsumesUnderlyingActions(t *testing.T) {
	t.Run("action keys consumed without state change", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		feedKey(m, questionKey())
		if !m.help {
			t.Fatal("\"?\" did not open help")
		}
		before := m.cursor
		follow := m.stream.Following()
		split := m.split
		for _, key := range []string{"j", "k", "up", "down", "f", "g", "[", "]", "0", "enter", "/", "x", "p", "r", "d", "n", "y"} {
			var msg tea.KeyPressMsg
			switch key {
			case "up":
				msg = tea.KeyPressMsg{Code: tea.KeyUp}
			case "down":
				msg = tea.KeyPressMsg{Code: tea.KeyDown}
			default:
				msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
			}
			_, cmd := m.Update(msg)
			if cmd != nil {
				t.Fatalf("%q while help open returned a command (must consume)", key)
			}
		}
		if !m.help {
			t.Error("action keys closed the overlay; only ?/esc may close it")
		}
		if m.cursor != before || m.stream.Following() != follow || m.split != split ||
			m.detail || m.filterActive || m.filterQuery != "" || m.confirmKind != "" || m.notice != "" {
			t.Fatalf("help-open input mutated state: cursor %d follow %v split %v detail %v filter %v/%q confirm %q notice %q",
				m.cursor, m.stream.Following(), m.split, m.detail, m.filterActive, m.filterQuery, m.confirmKind, m.notice)
		}
		if pauses, resumes, drains, stops := acts.counts(); pauses+resumes+drains+len(stops) != 0 {
			t.Fatalf("help-open input dispatched lifecycle calls: p=%d r=%d d=%d stops=%v", pauses, resumes, drains, stops)
		}
		// Underline keys never entered the filter either: still idle.
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
		if m.help || m.filterActive || m.filterQuery != "" {
			t.Fatalf("esc left help=%v filter=%v query=%q", m.help, m.filterActive, m.filterQuery)
		}
	})

	t.Run("scroll keys move overlay not selection", func(t *testing.T) {
		m := newFixtureModel(t, 80, 12, true, 0)
		feedKey(m, questionKey())
		if got := len(m.helpRows(m.helpContentWidth())); got <= m.helpBodyHeight() {
			t.Fatalf("fixture has %d help rows for body %d: cannot pin scrolling", got, m.helpBodyHeight())
		}
		before := m.cursor
		m.helpScroll = 2
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyUp})
		if m.helpScroll != 1 || m.cursor != before {
			t.Fatalf("up: scroll=%d cursor=%d, want scroll 1 cursor %d", m.helpScroll, m.cursor, before)
		}
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
		if m.helpScroll != 2 || m.cursor != before {
			t.Fatalf("down: scroll=%d cursor=%d, want scroll 2 cursor %d", m.helpScroll, m.cursor, before)
		}
	})

	t.Run("ctrl+c quits while help open", func(t *testing.T) {
		m := newFilterModel(t, 120, 40)
		feedKey(m, questionKey())
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if cmd == nil {
			t.Fatal("ctrl+c while help open must quit")
		}
	})

	t.Run("confirmations ignore question mark", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		feedKey(m, tea.KeyPressMsg{Code: 'd', Text: "d"})
		if m.confirmKind != "drain" {
			t.Fatalf("confirmKind = %q, want drain", m.confirmKind)
		}
		feedKey(m, questionKey())
		if m.help {
			t.Error("\"?\" opened help while a confirmation was pending")
		}
		if m.confirmKind != "drain" {
			t.Fatalf("confirmKind = %q after \"?\", want drain still armed", m.confirmKind)
		}
		if pauses, resumes, drains, stops := acts.counts(); pauses+resumes+drains+len(stops) != 0 {
			t.Fatalf("\"?\" during confirm dispatched calls: %d %d %d %v", pauses, resumes, drains, stops)
		}
	})

	t.Run("background updates apply while help open", func(t *testing.T) {
		m := newFilterModel(t, 120, 40)
		feedKey(m, questionKey())
		mm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		m = mm.(*Model)
		if m.width != 100 || m.height != 30 || !m.help {
			t.Fatalf("resize while help open: %dx%d help=%v", m.width, m.height, m.help)
		}
		before := len(m.cards)
		m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
		if len(m.cards) != before || !m.help {
			t.Fatalf("snapshot while help open: cards=%d help=%v", len(m.cards), m.help)
		}
		m.applyTailBatch(nil)
		m.actionBusy = false
		mm, _ = m.Update(hostActionResultMsg{kind: "pause", ok: true, message: "paused"})
		m = mm.(*Model)
		if !m.help || !strings.Contains(m.notice, "paused") {
			t.Fatalf("action result while help open: help=%v notice=%q", m.help, m.notice)
		}
	})

	t.Run("mouse messages are ignored without action", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a", "sess-b"), acts, 0)
		before := m.cursor
		for _, msg := range []tea.Msg{
			tea.MouseClickMsg{X: 1, Y: 1},
			tea.MouseWheelMsg{X: 1, Y: 1},
			tea.MouseMotionMsg{X: 1, Y: 1},
			tea.MouseReleaseMsg{X: 1, Y: 1},
		} {
			mm, cmd := m.Update(msg)
			m = mm.(*Model)
			if cmd != nil {
				t.Fatalf("%T returned a command (must ignore)", msg)
			}
		}
		if m.cursor != before || m.detail || m.help {
			t.Fatalf("mouse mutated state: cursor=%d detail=%v help=%v", m.cursor, m.detail, m.help)
		}
		if pauses, resumes, drains, stops := acts.counts(); pauses+resumes+drains+len(stops) != 0 {
			t.Fatalf("mouse dispatched lifecycle calls: %d %d %d %v", pauses, resumes, drains, stops)
		}
		feedKey(m, questionKey())
		mm, cmd := m.Update(tea.MouseClickMsg{X: 2, Y: 2})
		m = mm.(*Model)
		if cmd != nil {
			t.Fatal("mouse while help open returned a command")
		}
		if !m.help || m.cursor != before {
			t.Fatalf("mouse while help open: help=%v cursor=%d", m.help, m.cursor)
		}
	})
}

// TestModel_HelpOverlayBoundsFrames pins AC-3 through production View:
// 40/80/120/200-column plain/styled frames with Unicode and overflowing
// sessions fit their dimensions; scrolling short help reaches every row;
// the footer keeps "?" at 40 columns beside long filters and notices.
// Removing the width/scroll bounds (the mutant) overflows a frame.
func TestModel_HelpOverlayBoundsFrames(t *testing.T) {
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{false, true} {
			t.Run(helpFrameName(width, plain), func(t *testing.T) {
				m := newFixtureModel(t, width, 40, plain, 1)
				feedKey(m, questionKey())
				raw := m.render()
				lines := strings.Split(raw, "\n")
				if len(lines) != 40 {
					t.Fatalf("%dx40: help frame has %d rows", width, len(lines))
				}
				for i, line := range lines {
					if w := lipgloss.Width(line); w > width {
						t.Fatalf("%dx40 row %d: width %d: %q", width, i, w, ansi.Strip(line))
					}
				}
				if plain && strings.Contains(raw, "\x1b") {
					t.Fatal("plain help frame carries an escape")
				}
				stripped := ansi.Strip(raw)
				for _, want := range []string{"keyboard help", "? help"} {
					if !strings.Contains(stripped, want) {
						t.Errorf("%dx40 plain=%v: frame missing %q", width, plain, want)
					}
				}
			})
		}
	}

	t.Run("short help scrolls to every row", func(t *testing.T) {
		m := newFixtureModel(t, 80, 10, true, 0)
		feedKey(m, questionKey())
		// Every help row must have been reachable in the scroll window.
		rows := m.helpRows(m.helpContentWidth())
		m.helpScroll = 0
		m.clampHelpScroll()
		reachable := map[string]bool{}
		for {
			start, bodyH := m.helpScroll, m.helpBodyHeight()
			end := start + bodyH
			if end > len(rows) {
				end = len(rows)
			}
			for _, r := range rows[start:end] {
				reachable[r] = true
			}
			next := m.helpScroll
			feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
			if m.helpScroll == next {
				break
			}
		}
		for _, r := range rows {
			if !reachable[r] {
				t.Fatalf("help row never reachable by scrolling: %q", r)
			}
		}
		// A resize clamps an overshot scroll offset back into range.
		m.helpScroll = 1000
		mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
		m = mm.(*Model)
		if m.helpScroll > len(rows) {
			t.Fatalf("resize left scroll %d beyond %d rows", m.helpScroll, len(rows))
		}
		view := m.render()
		for i, line := range strings.Split(view, "\n") {
			if w := lipgloss.Width(line); w > 80 {
				t.Fatalf("clamped scroll row %d width %d: %q", i, w, line)
			}
		}
	})

	t.Run("minimum 80x6 help shows entries", func(t *testing.T) {
		for _, plain := range []bool{false, true} {
			m := newFixtureModel(t, 80, 6, plain, 0)
			feedKey(m, questionKey())
			if !m.help {
				t.Fatalf("plain=%v: \"?\" did not open help at 80x6", plain)
			}
			if got := m.helpBodyHeight(); got < 2 {
				t.Fatalf("plain=%v: 80x6 help body height=%d, want >=2", plain, got)
			}
			rows := m.helpRows(m.helpContentWidth())
			reachable := map[string]bool{}
			for {
				start, bodyH := m.helpScroll, m.helpBodyHeight()
				end := start + bodyH
				if end > len(rows) {
					end = len(rows)
				}
				for _, r := range rows[start:end] {
					reachable[r] = true
				}
				next := m.helpScroll
				feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
				if m.helpScroll == next {
					break
				}
			}
			for _, r := range rows {
				if !reachable[r] {
					t.Fatalf("plain=%v: 80x6 help row never reachable: %q", plain, r)
				}
			}
			// Every scrolled frame stays bounded and keeps chrome.
			m.helpScroll = 0
			m.clampHelpScroll()
			for {
				raw := m.render()
				lines := strings.Split(raw, "\n")
				if len(lines) != 6 {
					t.Fatalf("plain=%v: 80x6 help frame has %d rows, want 6", plain, len(lines))
				}
				for i, line := range lines {
					if w := lipgloss.Width(line); w > 80 {
						t.Fatalf("plain=%v: 80x6 row %d width %d", plain, i, w)
					}
				}
				if plain && strings.Contains(raw, "\x1b") {
					t.Fatal("plain 80x6 help frame carries an escape")
				}
				next := m.helpScroll
				feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
				if m.helpScroll == next {
					break
				}
			}
			// First and last entries each appear in some frame.
			seenFirst, seenLast := false, false
			m.helpScroll = 0
			m.clampHelpScroll()
			for {
				view := ansi.Strip(m.render())
				if strings.Contains(view, "select a session") {
					seenFirst = true
				}
				if strings.Contains(view, "mouse") {
					seenLast = true
				}
				next := m.helpScroll
				feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
				if m.helpScroll == next {
					break
				}
			}
			if !seenFirst || !seenLast {
				t.Fatalf("plain=%v: 80x6 frames missing entries first=%v last=%v", plain, seenFirst, seenLast)
			}
		}
	})

	t.Run("footer keeps anchor beside long filters and notices", func(t *testing.T) {
		m := newFilterModel(t, 40, 40)
		feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
		feedQuery(m, "a-very-long-filter-query-that-overflows-forty-columns")
		lines := strings.Split(ansi.Strip(m.render()), "\n")
		last := lines[len(lines)-1]
		if !strings.Contains(last, "? help") {
			t.Errorf("40-col footer lost the help anchor beside a long filter: %q", last)
		}
		if w := lipgloss.Width(last); w > 40 {
			t.Errorf("40-col footer width %d: %q", w, last)
		}
		m2 := newFixtureModel(t, 40, 40, true, 0)
		m2.notice = strings.Repeat("n", 120)
		lines = strings.Split(ansi.Strip(m2.render()), "\n")
		last = lines[len(lines)-1]
		if !strings.Contains(last, "? help") {
			t.Errorf("40-col footer lost the help anchor beside a long notice: %q", last)
		}
		if w := lipgloss.Width(last); w > 40 {
			t.Errorf("40-col notice footer width %d: %q", w, last)
		}
	})
}

// TestModel_ConfirmPromptKeepsActionWithoutHelpAnchor pins the destructive
// prompt over the idle help anchor: while a stop/drain confirmation is
// armed the footer keeps its leading action words ("stop", "drain host")
// and never advertises "? help" (confirmation handling swallows "?").
// Rebuilding the footer from the anchor (the mutant) truncates the head so
// the 40-column stop prompt loses "stop" and the 80-column drain prompt
// loses "drain host". Driven through production Update/View.
func TestModel_ConfirmPromptKeepsActionWithoutHelpAnchor(t *testing.T) {
	t.Run("40-col stop keeps action", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("session-xyz"), acts, 0)
		mm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 40})
		m = mm.(*Model)
		feedKey(m, tea.KeyPressMsg{Code: 'x', Text: "x"})
		if m.confirmKind != "stop" {
			t.Fatalf("confirmKind = %q, want stop", m.confirmKind)
		}
		last := strings.Split(ansi.Strip(m.render()), "\n")
		prompt := last[len(last)-1]
		if !strings.Contains(prompt, "stop") {
			t.Errorf("armed stop prompt lost action: %q", prompt)
		}
		if strings.Contains(prompt, "? help") {
			t.Errorf("confirm footer advertises help that confirmation swallows: %q", prompt)
		}
		if w := lipgloss.Width(prompt); w > 40 {
			t.Errorf("40-col stop footer width %d: %q", w, prompt)
		}
	})
	t.Run("80-col drain keeps action", func(t *testing.T) {
		acts := newFakeHostActions()
		m := newActionTestModel(t, actionTestSessions("sess-a"), acts, 0)
		mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
		m = mm.(*Model)
		feedKey(m, tea.KeyPressMsg{Code: 'd', Text: "d"})
		if m.confirmKind != "drain" {
			t.Fatalf("confirmKind = %q, want drain", m.confirmKind)
		}
		last := strings.Split(ansi.Strip(m.render()), "\n")
		prompt := last[len(last)-1]
		if !strings.Contains(prompt, "drain host") {
			t.Errorf("armed drain prompt lost action: %q", prompt)
		}
		if strings.Contains(prompt, "? help") {
			t.Errorf("confirm footer advertises help that confirmation swallows: %q", prompt)
		}
		if w := lipgloss.Width(prompt); w > 80 {
			t.Errorf("80-col drain footer width %d: %q", w, prompt)
		}
	})
}

func helpFrameName(width int, plain bool) string {
	mode := "styled"
	if plain {
		mode = "plain"
	}
	return strings.Join([]string{itoa(width), mode}, "-")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestModel_HelpOverlaySourceUnavailable pins the overlay over the
// daemon-unreachable frame: "?" still opens bounded help above the
// warning instead of replacing it.
func TestModel_HelpOverlaySourceUnavailable(t *testing.T) {
	m := New(Options{Plain: true, Now: func() time.Time { return fixtureNow }})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m = mm.(*Model)
	m.applySnapshot(Snapshot{Err: errFake("connection refused")})
	feedKey(m, questionKey())
	if !m.help {
		t.Fatal("\"?\" did not open help over the unreachable frame")
	}
	raw := m.render()
	stripped := ansi.Strip(raw)
	if !strings.Contains(stripped, "daemon unreachable") {
		t.Errorf("unreachable warning lost under help:\n%s", stripped)
	}
	if !strings.Contains(stripped, "keyboard help") {
		t.Errorf("help overlay missing over unreachable frame:\n%s", stripped)
	}
	lines := strings.Split(raw, "\n")
	if len(lines) != 20 {
		t.Fatalf("unreachable help frame has %d rows, want 20", len(lines))
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("row %d width %d: %q", i, w, line)
		}
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.help || !strings.Contains(ansi.Strip(m.render()), "daemon unreachable") {
		t.Fatalf("esc left help=%v or lost the warning", m.help)
	}
}
