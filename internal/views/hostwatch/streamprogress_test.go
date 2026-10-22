package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

var progressNow = time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC)

// newProgressModel is a model holding one live card, fed by applyTailBatch.
func newProgressModel(plain bool) *Model {
	m := New(Options{Plain: plain, Now: func() time.Time { return progressNow }})
	m.width, m.height = 120, 40
	m.cards = []SessionCard{{SessionID: "sess-1", IssueIdentifier: "ENG-1", DaemonState: "running"}}
	return m
}

func progressEvents(n int) []TailEvent {
	evs := make([]TailEvent, n)
	for i := range evs {
		evs[i] = TailEvent{SessionID: "sess-1", Event: agent.StreamProgressEvent(), At: progressNow}
	}
	return evs
}

func TestStreamProgress_NeverAppendsLines(t *testing.T) {
	for _, plain := range []bool{false, true} {
		m := newProgressModel(plain)
		batch := append(progressEvents(100), TailEvent{
			SessionID: "sess-1", Event: agent.AssistantTextEvent{Text: "done thinking"}, At: progressNow,
		})
		m.applyTailBatch(batch)
		lines := m.stream.lines
		if len(lines) != 1 || !strings.Contains(lines[0], "done thinking") {
			t.Fatalf("plain=%v: want exactly one content line, got %d: %q", plain, len(lines), lines)
		}
	}
}

func TestStreamProgress_ContentFreeSubtypesHidden(t *testing.T) {
	for _, sub := range []string{
		agent.SystemSubtypeStreamProgress,
		agent.SystemSubtypeModelIdentity,
		agent.SystemSubtypeGenerationID,
	} {
		m := newProgressModel(true)
		m.applyTailBatch([]TailEvent{{SessionID: "sess-1", Event: agent.SystemEvent{Subtype: sub}, At: progressNow}})
		if m.stream.total != 0 {
			t.Errorf("subtype %q appended %d stream lines", sub, m.stream.total)
		}
	}
}

func TestStreamProgress_AdvancesIndicatorFrame(t *testing.T) {
	m := newProgressModel(false)
	var prev string
	for i := 1; i <= 3; i++ {
		m.applyTailBatch(progressEvents(1))
		ind := m.cards[0].streamingIndicator(progressNow, false)
		if want := streamFrames[i%len(streamFrames)]; !strings.HasPrefix(ind, want) {
			t.Fatalf("tick %d: indicator %q, want frame %q", i, ind, want)
		}
		if ind == prev {
			t.Fatalf("tick %d: indicator did not advance from %q", i, prev)
		}
		prev = ind
		if m.stream.total != 0 {
			t.Fatalf("tick %d: progress appended %d lines", i, m.stream.total)
		}
	}
	if !strings.Contains(m.render(), "streaming") {
		t.Fatalf("card does not show the streaming indicator:\n%s", m.render())
	}
}

func TestStreamProgress_IndicatorClears(t *testing.T) {
	cases := []struct {
		name  string
		event agent.Event
		now   time.Time
	}{
		{"assistant", agent.AssistantTextEvent{Text: "hi"}, progressNow},
		{"tool_use", agent.ToolUseEvent{ToolName: "Bash"}, progressNow},
		{"tool_result", agent.ToolResultEvent{ToolName: "Bash"}, progressNow},
		{"result", agent.ResultEvent{Success: true}, progressNow},
		{"stall", nil, progressNow.Add(streamStallAfter)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newProgressModel(false)
			m.applyTailBatch(progressEvents(5))
			if m.cards[0].streamingIndicator(progressNow, false) == "" {
				t.Fatal("indicator not shown while streaming")
			}
			if tc.event != nil {
				m.applyTailBatch([]TailEvent{{SessionID: "sess-1", Event: tc.event, At: progressNow}})
			}
			if got := m.cards[0].streamingIndicator(tc.now, false); got != "" {
				t.Fatalf("indicator %q still shown", got)
			}
		})
	}
}

func TestStreamProgress_PlainOutputStable(t *testing.T) {
	m := newProgressModel(true)
	m.applyTailBatch(progressEvents(1))
	first := m.render()
	m.applyTailBatch(progressEvents(50))
	if got := m.render(); got != first {
		t.Fatalf("plain view changed per progress delta:\n%s\n---\n%s", first, got)
	}
	if strings.Contains(first, agent.SystemSubtypeStreamProgress) {
		t.Fatalf("plain view prints progress lines:\n%s", first)
	}
}
