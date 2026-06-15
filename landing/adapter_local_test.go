package landing

import (
	"context"
	"strings"
	"testing"
)

// ghReply builds a fakeRunner reply func that returns the given JSON for any
// `gh pr view` call, and errors otherwise.
func ghReply(json string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		if strings.Contains(line, "gh pr view") {
			return json, nil
		}
		return "", nil
	}
}

func TestLocalAdapterName(t *testing.T) {
	a := NewLocalAdapter("org1", newFakeStorage())
	if a.Name() != "local" {
		t.Errorf("Name() = %q, want local", a.Name())
	}
}

func TestLocalAdapterCanEnqueue(t *testing.T) {
	tests := []struct {
		name  string
		reply func(string, []string) (string, error)
		want  bool
	}{
		{"open is eligible", ghReply(`{"state":"OPEN","headRefName":"b"}`), true},
		{"merged is not", ghReply(`{"state":"MERGED","headRefName":"b"}`), false},
		{"closed is not", ghReply(`{"state":"CLOSED","headRefName":"b"}`), false},
		{"gh failure denies", errReply("gh pr view", "boom", ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewLocalAdapter("org1", newFakeStorage())
			a.runner = &fakeRunner{reply: tt.reply}
			got, err := a.CanEnqueue(context.Background(), "owner", "repo", 1)
			if err != nil {
				t.Fatalf("CanEnqueue: %v", err)
			}
			if got != tt.want {
				t.Errorf("CanEnqueue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLocalAdapterEnqueueResolvesIssueID(t *testing.T) {
	st := newFakeStorage()
	a := NewLocalAdapter("org1", st)
	a.runner = &fakeRunner{reply: ghReply(`{"headRefName":"feature/ABC-1153-stuff","url":"https://x/pull/1","title":"do thing"}`)}

	status, err := a.Enqueue(context.Background(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Newly queued, position 1 → merging.
	if status.State != StateMerging || status.Position != 1 {
		t.Errorf("status = %+v, want merging at position 1", status)
	}
	if len(st.queue) != 1 {
		t.Fatalf("queue len = %d, want 1", len(st.queue))
	}
	e := st.queue[0]
	if e.IssueID != "ABC-1153" {
		t.Errorf("issueID = %q, want ABC-1153 (from branch)", e.IssueID)
	}
	if e.SourceBranch != "feature/ABC-1153-stuff" {
		t.Errorf("sourceBranch = %q", e.SourceBranch)
	}
	if e.OrgID != "org1" || e.RepoID != "owner/repo" {
		t.Errorf("entry key fields = (%q,%q), want (org1, owner/repo)", e.OrgID, e.RepoID)
	}
	if e.Priority != defaultPriority {
		t.Errorf("priority = %d, want %d", e.Priority, defaultPriority)
	}
}

func TestLocalAdapterEnqueueIssueIDFromTitle(t *testing.T) {
	st := newFakeStorage()
	a := NewLocalAdapter("org1", st)
	// Branch has no id; title does.
	a.runner = &fakeRunner{reply: ghReply(`{"headRefName":"random-branch","url":"https://x/pull/1","title":"XYZ-42: fix"}`)}
	if _, err := a.Enqueue(context.Background(), "owner", "repo", 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if st.queue[0].IssueID != "XYZ-42" {
		t.Errorf("issueID = %q, want XYZ-42 (from title)", st.queue[0].IssueID)
	}
}

func TestLocalAdapterEnqueueFallbackIssueID(t *testing.T) {
	st := newFakeStorage()
	a := NewLocalAdapter("org1", st)
	a.runner = &fakeRunner{reply: ghReply(`{"headRefName":"plain","url":"https://x/pull/9","title":"no id"}`)}
	if _, err := a.Enqueue(context.Background(), "owner", "repo", 9); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if st.queue[0].IssueID != "PR-9" {
		t.Errorf("issueID = %q, want PR-9 fallback", st.queue[0].IssueID)
	}
}

func TestLocalAdapterEnqueueIdempotent(t *testing.T) {
	st := newFakeStorage()
	a := NewLocalAdapter("org1", st)
	a.runner = &fakeRunner{reply: ghReply(`{"headRefName":"ABC-1","url":"u","title":"t"}`)}
	if _, err := a.Enqueue(context.Background(), "owner", "repo", 1); err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	if _, err := a.Enqueue(context.Background(), "owner", "repo", 1); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	if len(st.queue) != 1 {
		t.Errorf("queue len = %d, want 1 (idempotent)", len(st.queue))
	}
}

func TestLocalAdapterGetStatus(t *testing.T) {
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}

	t.Run("queued position 2", func(t *testing.T) {
		st := newFakeStorage()
		_ = st.Enqueue(ctx, Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 1})
		_ = st.Enqueue(ctx, Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 2})
		a := NewLocalAdapter("org1", st)
		got, _ := a.GetStatus(ctx, "owner", "repo", 2)
		if got.State != StateQueued || got.Position != 2 {
			t.Errorf("status = %+v, want queued at 2", got)
		}
	})

	t.Run("failed", func(t *testing.T) {
		st := newFakeStorage()
		_ = st.MarkFailed(ctx, key, 3, "tests broke")
		a := NewLocalAdapter("org1", st)
		got, _ := a.GetStatus(ctx, "owner", "repo", 3)
		if got.State != StateFailed || got.FailureReason != "tests broke" {
			t.Errorf("status = %+v, want failed", got)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		st := newFakeStorage()
		_ = st.MarkBlocked(ctx, key, 4, "conflict")
		a := NewLocalAdapter("org1", st)
		got, _ := a.GetStatus(ctx, "owner", "repo", 4)
		if got.State != StateBlocked || got.FailureReason != "conflict" {
			t.Errorf("status = %+v, want blocked", got)
		}
	})

	t.Run("merged via gh", func(t *testing.T) {
		st := newFakeStorage()
		a := NewLocalAdapter("org1", st)
		a.runner = &fakeRunner{reply: ghReply(`{"state":"MERGED"}`)}
		got, _ := a.GetStatus(ctx, "owner", "repo", 5)
		if got.State != StateMerged {
			t.Errorf("status = %+v, want merged", got)
		}
	})

	t.Run("not queued", func(t *testing.T) {
		st := newFakeStorage()
		a := NewLocalAdapter("org1", st)
		a.runner = &fakeRunner{reply: ghReply(`{"state":"OPEN"}`)}
		got, _ := a.GetStatus(ctx, "owner", "repo", 6)
		if got.State != StateNotQueued {
			t.Errorf("status = %+v, want not-queued", got)
		}
	})
}

func TestLocalAdapterDequeue(t *testing.T) {
	ctx := context.Background()
	st := newFakeStorage()
	_ = st.Enqueue(ctx, Entry{OrgID: "org1", RepoID: "owner/repo", Proposal: 1})
	a := NewLocalAdapter("org1", st)
	if err := a.Dequeue(ctx, "owner", "repo", 1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if ok, _ := st.IsEnqueued(ctx, Key{OrgID: "org1", RepoID: "owner/repo"}, 1); ok {
		t.Error("Dequeue should remove the proposal")
	}
}

func TestLocalAdapterIsEnabled(t *testing.T) {
	a := NewLocalAdapter("org1", newFakeStorage())
	ok, err := a.IsEnabled(context.Background(), "owner", "repo")
	if err != nil || !ok {
		t.Errorf("IsEnabled = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestLocalAdapterRejectsEmptyOrg(t *testing.T) {
	a := NewLocalAdapter("", newFakeStorage())
	if _, err := a.Enqueue(context.Background(), "owner", "repo", 1); err == nil {
		t.Error("Enqueue with empty orgID should error (invalid key)")
	}
	if _, err := a.GetStatus(context.Background(), "owner", "repo", 1); err == nil {
		t.Error("GetStatus with empty orgID should error")
	}
}
