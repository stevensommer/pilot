package autopilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qf-studio/pilot/internal/testutil"
	github "github.com/qf-studio/studio-sdk/sdk/integrations/github"
)

// GH-17: a direct follow-up to GH-15/#16. #16 correctly stopped the poller
// from blindly re-dispatching a source issue once a live fix issue already
// owns the continuation, by stripping the "pilot" dispatch label — but it
// only ever relabeled the source issue, never closed it. That exposes a
// pre-existing gap: CreateFailureIssue/CreateReviewIssue (feedback_loop.go)
// write a literal "Depends on: #<source>" line into every fix issue's body,
// and internal/executor/base_presence.go's dispatch-time guard holds the fix
// issue until that referenced source issue's own state reports closed (or
// its linked PR reports merged). Since the source was only ever relabeled,
// that condition never became true and the fix issue was held forever — #16
// converted silent duplicate-PR waste into a silent, permanent deadlock.
//
// TestGH17_ClosesSourceIssueWhenFixIssueIsLive proves the fix: closing (not
// just relabeling) the source issue the moment a live fix issue is
// confirmed, with a comment tracing to that fix issue. The negative control
// (TestGH17_LeavesSourceIssueOpenWhenNoFixIssueOwns) proves genuinely
// terminal failures with no spawned fix issue keep their existing
// retriable-while-open behavior unchanged. TestGH17_RearmReopensClosedSource
// covers the other half of the acceptance criteria: owner_death.go's
// rearmDeadOwnerSource must reopen (not just relabel) a source that was
// closed for a fix-issue takeover, if that fix issue later dies before
// shipping — otherwise the work would be stranded with no path back into the
// poller's queue at all.
func TestGH17_ClosesSourceIssueWhenFixIssueIsLive(t *testing.T) {
	const (
		prNumber    = 9801
		issueNumber = 9802
		fixIssueNum = 9803
	)

	var closedStates []string
	var comments []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/issues/9802" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: issueNumber, State: github.StateOpen}))
		case r.URL.Path == "/repos/owner/repo/issues/9803" && r.Method == http.MethodGet:
			// The designated fix issue is open and alive.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: fixIssueNum, State: github.StateOpen}))
		case r.URL.Path == "/repos/owner/repo/issues/9802" && r.Method == http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			closedStates = append(closedStates, body["state"])
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: issueNumber, State: body["state"]}))
		case r.URL.Path == "/repos/owner/repo/issues/9802/comments" && r.Method == http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			comments = append(comments, body["body"])
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Comment{ID: 1, Body: body["body"]}))
		case r.URL.Path == "/repos/owner/repo/issues/9802/labels" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]github.Label{})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/9802/labels/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer server.Close()

	ghClient := github.NewClientWithBaseURL(testutil.FakeGitHubToken, server.URL)
	cfg := DefaultConfig()
	cfg.Environment = EnvDev

	store := newTestStateStore(t)
	dedupKey := spawnedFixDedupKey(prNumber, FailureCIPreMerge, nil)
	if _, err := store.ClaimSpawnedFix("owner/repo", dedupKey); err != nil {
		t.Fatalf("ClaimSpawnedFix: %v", err)
	}
	if err := store.RecordSpawnedFixIssue("owner/repo", dedupKey, fixIssueNum); err != nil {
		t.Fatalf("RecordSpawnedFixIssue: %v", err)
	}

	controller := NewController(cfg, ghClient, nil, "owner", "repo")
	controller.SetStateStore(store)

	prState := &PRState{
		PRNumber:      prNumber,
		PRURL:         "https://github.com/owner/repo/pull/9801",
		IssueNumber:   issueNumber,
		Stage:         StageFailed,
		Error:         "CI fix iteration limit reached",
		TerminalLabel: github.LabelFailed,
	}

	controller.notifyExternalClose(context.Background(), prState)

	if len(closedStates) == 0 {
		t.Fatalf("expected source issue #%d to be closed via UpdateIssueState while fix issue #%d is alive, but it was never PATCHed", issueNumber, fixIssueNum)
	}
	if got := closedStates[0]; got != github.StateClosed {
		t.Errorf("source issue state PATCH = %q, want %q", got, github.StateClosed)
	}

	foundTraceComment := false
	for _, c := range comments {
		if strings.Contains(c, "#9803") {
			foundTraceComment = true
		}
	}
	if !foundTraceComment {
		t.Errorf("expected a comment on the source issue tracing the closure to fix issue #%d, comments posted: %v", fixIssueNum, comments)
	}
}

// TestGH17_LeavesSourceIssueOpenWhenNoFixIssueOwns is the negative control:
// a genuinely-terminal pilot-failed close (no fix issue was ever spawned —
// the iteration-limit/CI-timeout/consecutive-API-failure branches) must NOT
// close the source issue. That issue remains the sole owner of its own
// retry via the normal pilot-failed-retry-N ladder, which requires it to
// stay open and pollable.
func TestGH17_LeavesSourceIssueOpenWhenNoFixIssueOwns(t *testing.T) {
	const (
		prNumber    = 9901
		issueNumber = 9902
	)

	var closedStates []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/issues/9902" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: issueNumber, State: github.StateOpen}))
		case r.URL.Path == "/repos/owner/repo/issues/9902" && r.Method == http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			closedStates = append(closedStates, body["state"])
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/owner/repo/issues/9902/labels" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]github.Label{})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/9902/labels/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer server.Close()

	ghClient := github.NewClientWithBaseURL(testutil.FakeGitHubToken, server.URL)
	cfg := DefaultConfig()
	cfg.Environment = EnvDev

	store := newTestStateStore(t)
	// No ClaimSpawnedFix/RecordSpawnedFixIssue for this PR — genuinely no
	// fix issue was ever spawned (e.g. the iteration-limit cascade-stop).

	controller := NewController(cfg, ghClient, nil, "owner", "repo")
	controller.SetStateStore(store)

	prState := &PRState{
		PRNumber:      prNumber,
		PRURL:         "https://github.com/owner/repo/pull/9901",
		IssueNumber:   issueNumber,
		Stage:         StageFailed,
		Error:         "CI fix iteration limit reached (10/10): stopping cascade to prevent infinite loop",
		TerminalLabel: github.LabelFailed,
	}

	controller.notifyExternalClose(context.Background(), prState)

	for _, s := range closedStates {
		if s == github.StateClosed {
			t.Errorf("source issue #%d must NOT be closed when no fix issue owns the retry — it must stay open and retriable via its own pilot-failed-retry-N ladder", issueNumber)
		}
	}
}

// TestGH17_RearmReopensClosedSource proves owner_death.go's
// rearmDeadOwnerSource reopens (not merely relabels) a source issue that was
// closed for a fix-issue takeover, once that designated fix issue itself
// dies before shipping. Without this, a fix issue dying after its source was
// closed (rather than merely relabeled, per GH-17) would strand the work
// with no path back into the poller's queue at all — worse than either of
// the two behaviors GH-15/GH-16/GH-17 set out to fix.
func TestGH17_RearmReopensClosedSource(t *testing.T) {
	const (
		sourceIssueNum = 9601
		fixIssueNum    = 9602
		originatingPR  = 9600
	)

	var reopenedStates []string
	var comments []string

	deadFixIssueBody := "Fix issue body.\n" +
		"<!-- autopilot-meta branch:pilot/GH-9601 pr:9600 iteration:1 -->\n" +
		"Depends on: #9601\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/issues/9602" && r.Method == http.MethodGet:
			// The designated fix issue died: closed without ever shipping
			// (no pilot-done label).
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{
				Number: fixIssueNum,
				State:  github.StateClosed,
				Body:   deadFixIssueBody,
			}))
		case r.URL.Path == "/repos/owner/repo/issues/9601" && r.Method == http.MethodGet:
			// The source issue was closed by GH-17's fix-issue-takeover
			// closure — still carrying pilot-failed, no retry-exhausted.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{
				Number: sourceIssueNum,
				State:  github.StateClosed,
				Labels: []github.Label{{Name: github.LabelFailed}},
			}))
		case r.URL.Path == "/repos/owner/repo/issues/9601" && r.Method == http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			reopenedStates = append(reopenedStates, body["state"])
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/owner/repo/issues/9601/comments" && r.Method == http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			comments = append(comments, body["body"])
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Comment{ID: 1, Body: body["body"]}))
		case r.URL.Path == "/repos/owner/repo/issues/9601/labels" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]github.Label{})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/9601/labels/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer server.Close()

	ghClient := github.NewClientWithBaseURL(testutil.FakeGitHubToken, server.URL)
	cfg := DefaultConfig()
	cfg.Environment = EnvDev

	controller := NewController(cfg, ghClient, nil, "owner", "repo")

	controller.ReactToDeclinedFixIssue(context.Background(), fixIssueNum, "closed without shipping")

	if len(reopenedStates) == 0 {
		t.Fatalf("expected source issue #%d to be reopened via UpdateIssueState after its designated fix issue #%d died, but it was never PATCHed", sourceIssueNum, fixIssueNum)
	}
	if got := reopenedStates[0]; got != github.StateOpen {
		t.Errorf("source issue state PATCH = %q, want %q", got, github.StateOpen)
	}

	foundReopenMention := false
	for _, c := range comments {
		if strings.Contains(c, "reopened") {
			foundReopenMention = true
		}
	}
	if !foundReopenMention {
		t.Errorf("expected the re-arm comment to mention the issue was reopened, comments posted: %v", comments)
	}
}
