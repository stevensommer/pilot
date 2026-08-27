package autopilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghadapter "github.com/qf-studio/pilot/internal/adapters/github"
	"github.com/qf-studio/pilot/internal/testutil"
	github "github.com/qf-studio/studio-sdk/sdk/integrations/github"
)

// GH-15: two coupled bugs let a CI-fix/review-feedback cascade lose against
// cmd/pilot's independent GitHub SDK poller, which auto-retries a *failed*
// issue via pilot-retry-N labels the moment it sees pilot-failed — with no
// notion that a revision issue already exists to own the continued work:
//
//  1. parseAutopilotIteration only ever advances correctly when the NEXT
//     PR's prState.IssueNumber is the revision issue this cascade just
//     spawned, not a blind retry of the original issue (whose body never
//     carries an autopilot-meta marker at all). TestGH15_IterationAdvances...
//     proves the counter-writing side of that is already correct given a
//     properly-advancing IssueNumber, per the issue's "do not touch
//     parseAutopilotIteration/CreateFailureIssue/CreateReviewIssue" scope.
//  2. notifyExternalClose's designated-to-a-fix-issue path (issueLabel ==
//     github.LabelFailed via prState.TerminalLabel or the durable
//     HasSpawnedFixForPR fallback) left the source issue carrying both
//     pilot-failed AND pilot ("dispatch label") — fully visible to the
//     vendored studio-sdk poller's label-scoped fetchCandidates, which is
//     exactly what let it win the race in a live four-round reproduction
//     every single time. TestGH15_BlindRetryGuard_* proves the fix: pilot is
//     now stripped whenever a live fix issue durably owns the PR, and
//     restored by rearmDeadOwnerSource if that fix issue later dies.

// TestGH15_IterationAdvancesFromRevisionIssue proves that dispatching a
// CI-fix cascade from a revision issue whose own body already carries
// iteration:N produces a next-round revision issue with iteration:N+1, not
// iteration:1 — the correct-counter behavior the blind-retry race (fixed by
// TestGH15_BlindRetryGuard_* below) makes reachable in practice.
func TestGH15_IterationAdvancesFromRevisionIssue(t *testing.T) {
	const (
		prNumber      = 9501
		revisionIssue = 9502 // the "originating" issue for this PR — itself a prior revision issue
		nextFixIssue  = 9503
	)
	const codeLog = `Run golangci-lint run ./...
internal/autopilot/controller.go:1:1: some lint error (errcheck)
##[error]Process completed with exit code 1.`

	var createdBody string
	issueCreated := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/commits/gh15sha/check-runs":
			resp := github.CheckRunsResponse{
				TotalCount: 1,
				CheckRuns: []github.CheckRun{
					{ID: 601, Name: "lint", Status: "completed", Conclusion: "failure"},
				},
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, resp))
		case r.URL.Path == "/repos/owner/repo/actions/jobs/601/logs":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(codeLog))
		case r.URL.Path == "/repos/owner/repo/issues" && r.Method == http.MethodPost:
			var input github.IssueInput
			_ = json.NewDecoder(r.Body).Decode(&input)
			createdBody = input.Body
			issueCreated = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: nextFixIssue}))
		case r.URL.Path == "/repos/owner/repo/pulls/9501" && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/owner/repo/issues/9502" && r.Method == http.MethodGet:
			// The "originating" issue for this PR is itself a revision issue
			// spawned by an earlier round — its body already carries the
			// autopilot-meta iteration marker from that round.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{
				Number: revisionIssue,
				State:  github.StateOpen,
				Body:   "Some revision issue body.\n<!-- autopilot-meta branch:pilot/GH-9502 pr:9490 iteration:3 -->\n",
			}))
		case r.URL.Path == "/repos/owner/repo/issues/9502/labels" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]github.Label{})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/9502/labels/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer server.Close()

	ghClient := github.NewClientWithBaseURL(testutil.FakeGitHubToken, server.URL)
	stepClient := ghadapter.NewClientWithBaseURL(testutil.FakeGitHubToken, server.URL)
	cfg := DefaultConfig()
	cfg.Environment = EnvDev
	// Well above the iteration:3 the origin issue already carries, so this
	// round falls through to spawn the next revision issue instead of
	// hitting the iteration-limit cascade-stop.
	cfg.MaxCIFixIterations = 10

	store := newTestStateStore(t)

	controller := NewController(cfg, ghClient, nil, "owner", "repo", WithStepLogClient(stepClient))
	controller.SetStateStore(store)

	prState := &PRState{
		PRNumber:    prNumber,
		PRURL:       "https://github.com/owner/repo/pull/9501",
		IssueNumber: revisionIssue,
		BranchName:  "pilot/GH-9502",
		HeadSHA:     "gh15sha",
		Stage:       StageCIFailed,
		CreatedAt:   time.Now().Add(-1 * time.Hour),
	}

	if err := controller.handleCIFailed(context.Background(), prState); err != nil {
		t.Fatalf("handleCIFailed returned unexpected error: %v", err)
	}
	if !issueCreated {
		t.Fatal("expected a next-round revision issue to be spawned")
	}

	gotIteration := parseAutopilotIteration(createdBody)
	if gotIteration != 4 {
		t.Fatalf("next revision issue's iteration = %d, want 4 (origin issue #%d carried iteration:3) — body: %s",
			gotIteration, revisionIssue, createdBody)
	}
	if prState.TerminalLabel != github.LabelFailed {
		t.Fatalf("prState.TerminalLabel = %q, want %q — the origin issue must be designated to the new revision issue, not left for a blind retry",
			prState.TerminalLabel, github.LabelFailed)
	}
}

// TestGH15_BlindRetryGuard_StripsPilotLabelWhenFixIssueIsLive proves
// notifyExternalClose's fix for the actual race: when this PR close resolves
// to pilot-failed AND a fix issue is already durably designated (and alive)
// to own the continued work, the source issue's dispatch label ("pilot") is
// stripped in the very same mutation. cmd/pilot's vendored GitHub SDK poller
// (studio-sdk's poller.go) only ever lists issues carrying that label
// (fetchCandidates: ListIssues{Labels: [pilot]}) — an issue lacking it never
// reaches shouldRetryFailedIssue, so the "Auto-retrying pilot-failed issue"
// blind retry can no longer fire against it. This is the exact race a live
// four-round reproduction lost every single time before this fix.
func TestGH15_BlindRetryGuard_StripsPilotLabelWhenFixIssueIsLive(t *testing.T) {
	const (
		prNumber    = 9601
		issueNumber = 9602
		fixIssueNum = 9603
	)

	var labelsAdded []string
	var labelsRemoved []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/issues/9602" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: issueNumber, State: github.StateOpen}))
		case r.URL.Path == "/repos/owner/repo/issues/9603" && r.Method == http.MethodGet:
			// The designated fix issue is open and alive.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: fixIssueNum, State: github.StateOpen}))
		case r.URL.Path == "/repos/owner/repo/issues/9602/labels" && r.Method == http.MethodPost:
			var body map[string][]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			labelsAdded = append(labelsAdded, body["labels"]...)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]github.Label{})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/9602/labels/") && r.Method == http.MethodDelete:
			label := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/issues/9602/labels/")
			labelsRemoved = append(labelsRemoved, label)
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
		PRURL:         "https://github.com/owner/repo/pull/9601",
		IssueNumber:   issueNumber,
		Stage:         StageFailed,
		Error:         "CI fix iteration limit reached",
		TerminalLabel: github.LabelFailed,
	}

	controller.notifyExternalClose(context.Background(), prState)

	foundPilotRemoved := false
	for _, l := range labelsRemoved {
		if l == github.LabelPilot {
			foundPilotRemoved = true
		}
	}
	if !foundPilotRemoved {
		t.Errorf("expected the dispatch label %q to be removed from the source issue while its fix issue #%d is alive — labels removed: %v",
			github.LabelPilot, fixIssueNum, labelsRemoved)
	}
	for _, l := range labelsAdded {
		if l == github.LabelPilot {
			t.Errorf("dispatch label %q must not be re-added in the same mutation that designates the issue to a live fix issue — labels added: %v",
				github.LabelPilot, labelsAdded)
		}
	}

	foundFailed := false
	for _, l := range labelsAdded {
		if l == github.LabelFailed {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Errorf("expected source issue to still be labeled %q — labels added: %v", github.LabelFailed, labelsAdded)
	}
}

// TestGH15_BlindRetryGuard_LeavesPilotLabelWhenNoFixIssueOwns is the negative
// control: a genuinely-terminal pilot-failed close (no fix issue was ever
// spawned — e.g. the iteration-limit/CI-timeout/consecutive-API-failure
// branches in handleCIFailed/handleReviewRequested) must NOT have its pilot
// label stripped. That source issue is the sole owner of its own retry, and
// cmd/pilot's SDK poller's pilot-failed-retry-N ladder is exactly the
// intended mechanism for it — this must keep working unchanged.
func TestGH15_BlindRetryGuard_LeavesPilotLabelWhenNoFixIssueOwns(t *testing.T) {
	const (
		prNumber    = 9701
		issueNumber = 9702
	)

	var labelsAdded []string
	var labelsRemoved []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/issues/9702" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mustJSON(t, github.Issue{Number: issueNumber, State: github.StateOpen}))
		case r.URL.Path == "/repos/owner/repo/issues/9702/labels" && r.Method == http.MethodPost:
			var body map[string][]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			labelsAdded = append(labelsAdded, body["labels"]...)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]github.Label{})
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/9702/labels/") && r.Method == http.MethodDelete:
			label := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/issues/9702/labels/")
			labelsRemoved = append(labelsRemoved, label)
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
		PRURL:         "https://github.com/owner/repo/pull/9701",
		IssueNumber:   issueNumber,
		Stage:         StageFailed,
		Error:         "CI fix iteration limit reached (10/10): stopping cascade to prevent infinite loop",
		TerminalLabel: github.LabelFailed,
	}

	controller.notifyExternalClose(context.Background(), prState)

	for _, l := range labelsRemoved {
		if l == github.LabelPilot {
			t.Errorf("dispatch label %q must NOT be stripped when no fix issue owns the retry — this source issue is the sole owner and must stay pollable via its own pilot-failed-retry-N ladder; labels removed: %v",
				github.LabelPilot, labelsRemoved)
		}
	}

	foundFailed := false
	for _, l := range labelsAdded {
		if l == github.LabelFailed {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Errorf("expected source issue to still be labeled %q — labels added: %v", github.LabelFailed, labelsAdded)
	}
}
