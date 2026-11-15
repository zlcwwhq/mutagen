package core

import (
	"bytes"
	"testing"
)

// testingRepositoryCommit creates a commit identifier from a single byte.
func testingRepositoryCommit(value byte) []byte {
	return bytes.Repeat([]byte{value}, 20)
}

// testingRepository creates a repository with the specified branch and commit,
// along with settings that agreements don't record.
func testingRepository(branch string, commit byte) *Repository {
	return &Repository{
		Commit:   testingRepositoryCommit(commit),
		Branch:   branch,
		Url:      "https://example.org/repository.git",
		AutoCRLF: "input",
	}
}

// testingRepositoryModes are the synchronization modes, in the order in which
// tests list their expectations.
var testingRepositoryModes = []SynchronizationMode{
	SynchronizationMode_SynchronizationModeTwoWaySafe,
	SynchronizationMode_SynchronizationModeTwoWayResolved,
	SynchronizationMode_SynchronizationModeOneWaySafe,
	SynchronizationMode_SynchronizationModeOneWayReplica,
}

// repositoryExpectation describes the expected reconciliation of repositories.
type repositoryExpectation struct {
	// alpha and beta are the policies of the expected actions, if any, as
	// "follow", "ff", or "fff" (for RepositoryPolicy_FastForwardOrFollow).
	alpha, beta string
	// result is the expected result without actions: "unavailable" for no
	// reconciliation at all, "agreed" for an agreement, "conflict" for a
	// conflict, and otherwise nothing, which keeps the ancestor.
	result string
}

// describePolicy describes the policy of an action for repositoryExpectation.
func describePolicy(action *RepositoryAction) string {
	if action == nil {
		return ""
	}
	switch action.Policy {
	case RepositoryPolicy_Follow:
		return "follow"
	case RepositoryPolicy_FastForward:
		return "ff"
	case RepositoryPolicy_FastForwardOrFollow:
		return "fff"
	default:
		return "invalid"
	}
}

// TestReconcileRepository tests the reconciliation of repositories in every
// synchronization mode.
func TestReconcileRepository(t *testing.T) {
	main0, main1, main2 := testingRepository("main", 0), testingRepository("main", 1), testingRepository("main", 2)
	dev0, dev2 := testingRepository("dev", 0), testingRepository("dev", 2)
	detached1, detached2 := testingRepository("", 1), testingRepository("", 2)
	unborn := &Repository{Branch: "main"}
	unavailable := &Repository{Unavailable: "a rebase is in progress"}
	ancestor := agreement(main0)

	// Define the expectations that are the same in every mode.
	every := func(expectation repositoryExpectation) [4]repositoryExpectation {
		return [4]repositoryExpectation{expectation, expectation, expectation, expectation}
	}
	none := repositoryExpectation{}
	betaFollows := repositoryExpectation{beta: "follow"}
	agreed := repositoryExpectation{result: "agreed"}
	conflict := repositoryExpectation{result: "conflict"}
	unavailableResult := repositoryExpectation{result: "unavailable"}
	twoWayOnly := func(expectation repositoryExpectation) [4]repositoryExpectation {
		return [4]repositoryExpectation{expectation, expectation, none, none}
	}

	tests := []struct {
		description           string
		ancestor, alpha, beta *Repository
		expected              [4]repositoryExpectation
	}{
		{"alpha unavailable", ancestor, unavailable, main1, every(unavailableResult)},
		{"beta unavailable", ancestor, main1, unavailable, every(unavailableResult)},
		{"both unavailable", ancestor, unavailable, unavailable, every(unavailableResult)},
		{"unavailable without repository", nil, nil, unavailable, every(unavailableResult)},
		{"no repositories", nil, nil, nil, every(none)},
		{"no repositories after agreement", ancestor, nil, nil, every(none)},
		{"unborn repositories", nil, unborn, unborn, every(none)},
		{"unborn and no repository", nil, unborn, nil, every(none)},
		{"only alpha", nil, main1, nil, every(betaFollows)},
		{"only alpha after agreement", ancestor, main1, nil, every(betaFollows)},
		{"only alpha with commit", nil, main1, unborn, every(betaFollows)},
		{"only beta", nil, nil, main1, twoWayOnly(repositoryExpectation{alpha: "follow"})},
		{"only beta with commit", ancestor, unborn, main1, twoWayOnly(repositoryExpectation{alpha: "follow"})},
		{"agreement", ancestor, main1, main1, every(agreed)},
		{"agreement on ancestor", ancestor, main0, main0, every(agreed)},
		{"agreement without ancestor", nil, main1, main1, every(agreed)},
		{"agreement while detached", nil, detached1, detached1, every(agreed)},
		{"only alpha moved", ancestor, main1, main0, every(betaFollows)},
		{"only alpha switched branch", ancestor, dev0, main0, every(betaFollows)},
		{"only alpha detached", ancestor, detached1, main0, every(betaFollows)},
		{
			"only beta moved", ancestor, main0, main1,
			[4]repositoryExpectation{{alpha: "follow"}, {alpha: "follow"}, conflict, {beta: "follow"}},
		},
		{
			"only beta switched branch", ancestor, main0, dev0,
			[4]repositoryExpectation{{alpha: "follow"}, {alpha: "follow"}, conflict, {beta: "follow"}},
		},
		{
			"both moved on the same branch", ancestor, main1, main2,
			[4]repositoryExpectation{{alpha: "ff", beta: "ff"}, {alpha: "ff", beta: "fff"}, {beta: "ff"}, {beta: "follow"}},
		},
		{
			"both moved while detached", ancestor, detached1, detached2,
			[4]repositoryExpectation{{alpha: "ff", beta: "ff"}, {alpha: "ff", beta: "fff"}, {beta: "ff"}, {beta: "follow"}},
		},
		{
			"both moved to different branches", ancestor, main1, dev2,
			[4]repositoryExpectation{conflict, {beta: "follow"}, conflict, {beta: "follow"}},
		},
		{
			"both moved with one detached", ancestor, detached1, main2,
			[4]repositoryExpectation{conflict, {beta: "follow"}, conflict, {beta: "follow"}},
		},
		{
			"same branch without ancestor", nil, main1, main2,
			[4]repositoryExpectation{{alpha: "ff", beta: "ff"}, {alpha: "ff", beta: "ff"}, {beta: "ff"}, {beta: "ff"}},
		},
		{
			"detached without ancestor", nil, detached1, detached2,
			[4]repositoryExpectation{{alpha: "ff", beta: "ff"}, {alpha: "ff", beta: "ff"}, {beta: "ff"}, {beta: "ff"}},
		},
		{"different branches without ancestor", nil, main1, dev2, every(conflict)},
		{"different commits on different branches without ancestor", nil, main1, dev0, every(conflict)},
	}
	for _, test := range tests {
		for m, mode := range testingRepositoryModes {
			expected := test.expected[m]
			reconciliation := ReconcileRepository(test.ancestor, test.alpha, test.beta, mode)
			if reconciliation == nil {
				if expected.result != "unavailable" {
					t.Errorf("%s (%s): unexpected unavailability", test.description, mode.Description())
				}
				continue
			} else if expected.result == "unavailable" {
				t.Errorf("%s (%s): unexpected reconciliation", test.description, mode.Description())
				continue
			}

			// Verify the actions and their validity, and that each moves one
			// repository to the other's state, as it was scanned.
			if policy := describePolicy(reconciliation.AlphaAction); policy != expected.alpha {
				t.Errorf("%s (%s): incorrect alpha action: %q != %q", test.description, mode.Description(), policy, expected.alpha)
			}
			if policy := describePolicy(reconciliation.BetaAction); policy != expected.beta {
				t.Errorf("%s (%s): incorrect beta action: %q != %q", test.description, mode.Description(), policy, expected.beta)
			}
			for _, action := range []struct {
				action      *RepositoryAction
				old, target *Repository
			}{
				{reconciliation.AlphaAction, test.alpha, test.beta},
				{reconciliation.BetaAction, test.beta, test.alpha},
			} {
				if action.action == nil {
					continue
				} else if err := action.action.EnsureValid(); err != nil {
					t.Errorf("%s (%s): invalid action: %v", test.description, mode.Description(), err)
				} else if action.action.Old != action.old || action.action.Target != action.target {
					t.Errorf("%s (%s): action with incorrect repositories", test.description, mode.Description())
				}
			}

			// Verify the result without any actions being applied.
			newAncestor, newConflict := reconciliation.Complete(nil, nil)
			if err := newAncestor.ensureValidAncestor(); err != nil {
				t.Errorf("%s (%s): invalid ancestor: %v", test.description, mode.Description(), err)
			}
			if newConflict != nil {
				if err := newConflict.EnsureValid(); err != nil {
					t.Errorf("%s (%s): invalid conflict: %v", test.description, mode.Description(), err)
				} else if newConflict.Ancestor != test.ancestor || newConflict.Alpha != test.alpha || newConflict.Beta != test.beta {
					t.Errorf("%s (%s): conflict with incorrect repositories", test.description, mode.Description())
				}
			}
			switch expected.result {
			case "agreed":
				if !newAncestor.Equal(test.alpha) || newAncestor.Url != "" || newAncestor.AutoCRLF != "" || newConflict != nil {
					t.Errorf("%s (%s): agreement not recorded: %v, %v", test.description, mode.Description(), newAncestor, newConflict)
				}
			case "conflict":
				if newAncestor != test.ancestor || newConflict == nil {
					t.Errorf("%s (%s): conflict not reported: %v, %v", test.description, mode.Description(), newAncestor, newConflict)
				}
			default:
				if newAncestor != test.ancestor || newConflict != nil {
					t.Errorf("%s (%s): ancestor not kept: %v, %v", test.description, mode.Description(), newAncestor, newConflict)
				}
			}
		}
	}
}

// TestReconcileRepositoryComplete tests the determination of the ancestor and
// conflict from the results of actions.
func TestReconcileRepositoryComplete(t *testing.T) {
	main0, main1, main2 := testingRepository("main", 0), testingRepository("main", 1), testingRepository("main", 2)
	ancestor := agreement(main0)

	// Define the results.
	moved := &RepositoryResult{Outcome: RepositoryOutcome_Moved}
	movedWithWarning := &RepositoryResult{
		Outcome: RepositoryOutcome_Moved,
		Problem: &Problem{Path: ".git", Error: "staged changes were unstaged", Warning: true},
	}
	ahead := &RepositoryResult{Outcome: RepositoryOutcome_Ahead}
	diverged := &RepositoryResult{Outcome: RepositoryOutcome_Diverged}
	failed := &RepositoryResult{Outcome: RepositoryOutcome_Failed, Problem: &Problem{Path: ".git", Error: "missing objects"}}
	for _, result := range []*RepositoryResult{moved, movedWithWarning, ahead, diverged, failed} {
		if err := result.EnsureValid(); err != nil {
			t.Fatal("invalid result:", err)
		}
	}

	tests := []struct {
		description           string
		mode                  SynchronizationMode
		ancestor, alpha, beta *Repository
		alphaResult           *RepositoryResult
		betaResult            *RepositoryResult
		// expectedAncestor is the expected ancestor, which is the original one
		// if it's nil and agreed isn't set.
		expectedAncestor *Repository
		// conflict indicates that a conflict is expected.
		conflict bool
	}{
		// Two-way-safe fast-forwards.
		{"alpha fast-forwarded", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, moved, ahead, main2, false},
		{"beta fast-forwarded", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, ahead, moved, main1, false},
		{"beta fast-forwarded with warning", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, ahead, movedWithWarning, main1, false},
		{"diverged", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, diverged, diverged, nil, true},
		{"diverged on alpha only", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, diverged, failed, nil, true},
		{"ahead with failure", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, failed, ahead, nil, false},
		{"both failed", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, failed, failed, nil, false},
		{"not applied", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, nil, nil, nil, false},
		{"diverged without ancestor", SynchronizationMode_SynchronizationModeTwoWaySafe, nil, main1, main2, diverged, diverged, nil, true},
		{"both moved", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main2, moved, moved, nil, false},

		// Two-way-resolved, where alpha takes precedence.
		{"beta replaced", SynchronizationMode_SynchronizationModeTwoWayResolved, ancestor, main1, main2, diverged, moved, main1, false},
		{"alpha fast-forwarded with precedence", SynchronizationMode_SynchronizationModeTwoWayResolved, ancestor, main1, main2, moved, ahead, main2, false},
		{"diverged without precedence", SynchronizationMode_SynchronizationModeTwoWayResolved, nil, main1, main2, diverged, diverged, nil, true},

		// One-way-safe, where only beta has an action.
		{"beta fast-forwarded one way", SynchronizationMode_SynchronizationModeOneWaySafe, ancestor, main1, main2, nil, moved, main1, false},
		{"beta ahead one way", SynchronizationMode_SynchronizationModeOneWaySafe, ancestor, main1, main2, nil, ahead, nil, true},
		{"beta diverged one way", SynchronizationMode_SynchronizationModeOneWaySafe, ancestor, main1, main2, nil, diverged, nil, true},
		{"beta failed one way", SynchronizationMode_SynchronizationModeOneWaySafe, ancestor, main1, main2, nil, failed, nil, false},
		{"alpha result ignored", SynchronizationMode_SynchronizationModeOneWaySafe, ancestor, main1, main2, moved, failed, nil, false},

		// One-way-replica.
		{"beta replaced in replica", SynchronizationMode_SynchronizationModeOneWayReplica, ancestor, main1, main2, nil, moved, main1, false},
		{"beta ahead without ancestor", SynchronizationMode_SynchronizationModeOneWayReplica, nil, main1, main2, nil, ahead, nil, true},
		{"beta fast-forwarded without ancestor", SynchronizationMode_SynchronizationModeOneWayReplica, nil, main1, main2, nil, moved, main1, false},

		// Follows and attachments.
		{"beta followed", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main0, nil, moved, main1, false},
		{"alpha followed", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main0, main2, moved, nil, main2, false},
		{"follow failed", SynchronizationMode_SynchronizationModeTwoWaySafe, ancestor, main1, main0, nil, failed, nil, false},
		{"attached", SynchronizationMode_SynchronizationModeOneWaySafe, nil, main1, nil, nil, moved, main1, false},
		{"attachment failed", SynchronizationMode_SynchronizationModeOneWaySafe, nil, main1, nil, nil, failed, nil, false},
	}
	for _, test := range tests {
		reconciliation := ReconcileRepository(test.ancestor, test.alpha, test.beta, test.mode)
		if reconciliation == nil || (reconciliation.AlphaAction == nil && reconciliation.BetaAction == nil) {
			t.Errorf("%s: no actions", test.description)
			continue
		}
		newAncestor, newConflict := reconciliation.Complete(test.alphaResult, test.betaResult)
		if err := newAncestor.ensureValidAncestor(); err != nil {
			t.Errorf("%s: invalid ancestor: %v", test.description, err)
		}
		if test.expectedAncestor != nil {
			if !newAncestor.Equal(test.expectedAncestor) || newAncestor.Url != "" {
				t.Errorf("%s: incorrect ancestor: %v", test.description, newAncestor)
			}
		} else if newAncestor != test.ancestor {
			t.Errorf("%s: ancestor not kept: %v", test.description, newAncestor)
		}
		if test.conflict {
			if newConflict == nil {
				t.Errorf("%s: conflict not reported", test.description)
			} else if err := newConflict.EnsureValid(); err != nil {
				t.Errorf("%s: invalid conflict: %v", test.description, err)
			} else if newConflict.Ancestor != test.ancestor || newConflict.Alpha != test.alpha || newConflict.Beta != test.beta {
				t.Errorf("%s: conflict with incorrect repositories", test.description)
			}
		} else if newConflict != nil {
			t.Errorf("%s: unexpected conflict: %v", test.description, newConflict)
		}
	}
}

// TestReconcileRepositoryStaging tests that each endpoint that the session can
// change gets the other's commit when the repositories conflict without any
// actions, without moving to it, while reconciliations with actions only stage
// what those need.
func TestReconcileRepositoryStaging(t *testing.T) {
	ancestor := agreement(testingRepository("main", 0))
	alpha, beta := testingRepository("main", 1), testingRepository("topic", 2)
	for _, mode := range testingRepositoryModes {
		// Use repositories on different branches that both moved, which conflict
		// without actions unless alpha takes precedence.
		reconciliation := ReconcileRepository(nil, alpha, beta, mode)
		alphaStaging, betaStaging := reconciliation.Staging()
		if reconciliation.AlphaAction != nil || reconciliation.BetaAction != nil {
			t.Fatalf("unexpected actions in mode %v", mode)
		}
		oneWay := mode == SynchronizationMode_SynchronizationModeOneWaySafe ||
			mode == SynchronizationMode_SynchronizationModeOneWayReplica
		if oneWay != (alphaStaging == nil) {
			t.Errorf("incorrect staging on alpha in mode %v: %v", mode, alphaStaging)
		} else if alphaStaging != nil && (!alphaStaging.Target.Equal(beta) || alphaStaging.EnsureValid() != nil) {
			t.Errorf("invalid staging on alpha in mode %v: %v", mode, alphaStaging)
		}
		if betaStaging == nil || !betaStaging.Target.Equal(alpha) || betaStaging.EnsureValid() != nil {
			t.Errorf("invalid staging on beta in mode %v: %v", mode, betaStaging)
		}
		if _, conflict := reconciliation.Complete(nil, nil); conflict == nil {
			t.Errorf("staging affected conflict in mode %v", mode)
		}

		// With actions (here because only alpha moved), only their objects are
		// staged.
		reconciliation = ReconcileRepository(ancestor, alpha, testingRepository("main", 0), mode)
		alphaStaging, betaStaging = reconciliation.Staging()
		if alphaStaging != reconciliation.AlphaAction || betaStaging != reconciliation.BetaAction {
			t.Errorf("staging differs from actions in mode %v", mode)
		}
	}
}
