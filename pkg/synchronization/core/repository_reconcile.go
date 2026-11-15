package core

// RepositoryReconciliation is the result of reconciling the repositories at both
// endpoints' synchronization roots in Git mode (see ReconcileRepository). It
// consists of the actions to apply to them, if any, and determines the new
// ancestor and conflict once they've been applied (see Complete).
type RepositoryReconciliation struct {
	// AlphaAction is the action to apply to alpha's repository, if any.
	AlphaAction *RepositoryAction
	// BetaAction is the action to apply to beta's repository, if any.
	BetaAction *RepositoryAction
	// ancestor is the commit and branch on which the repositories last agreed,
	// or nil if they never have.
	ancestor *Repository
	// agreed is the commit and branch on which the repositories agree, if they
	// do.
	agreed *Repository
	// conflict describes the disagreement between the repositories, which is
	// reported as a conflict if the actions can't resolve it, or immediately
	// if there are none.
	conflict *RepositoryConflict
	// immediate indicates that the disagreement is a conflict without any
	// actions being applied.
	immediate bool
	// oneWay indicates that alpha never takes beta's commits, so that beta's
	// commit containing alpha's is a conflict rather than something that alpha
	// fast-forwards to.
	oneWay bool
}

// agreement returns the record of an agreement on a repository's commit and
// branch, which is what ancestors store.
func agreement(repository *Repository) *Repository {
	return &Repository{Commit: repository.Commit, Branch: repository.Branch}
}

// ReconcileRepository reconciles the repositories at both endpoints'
// synchronization roots in Git mode (see Snapshot.Repository) with the commit
// and branch on which they last agreed (the ancestor, which is nil if they
// never have), according to the synchronization mode. It returns nil if either
// repository is unavailable, in which case the session leaves both alone and
// keeps its ancestor and any conflict as they are until both can be compared
// again.
//
// Repositories that agree on their commit and branch become the new ancestor.
// If only one endpoint has a commit, then the other gets its commit and branch,
// with a repository attached if it has none (in one-way modes, only beta). If
// only one endpoint moved since the ancestor, then the other follows it, except
// that in the one-way-safe mode, beta moving is a conflict, while in the
// one-way-replica mode, beta goes back to alpha's commit. If both moved, or
// if they never agreed, then repositories on different branches conflict,
// except that the two-way-resolved and one-way-replica modes give alpha
// precedence if there's an ancestor, and repositories on the same branch are
// fast-forwarded to whichever commit contains the other, while commits that
// have diverged conflict, again unless alpha takes precedence. Without an
// ancestor, nothing ever follows anything but a fast-forward.
func ReconcileRepository(ancestor, alpha, beta *Repository, mode SynchronizationMode) *RepositoryReconciliation {
	// If either repository is unavailable, then nothing can be determined.
	if alpha.GetUnavailable() != "" || beta.GetUnavailable() != "" {
		return nil
	}

	// Set up the reconciliation. Any conflict is a disagreement between these
	// repositories, which can only exist if both of them have commits.
	oneWay := mode == SynchronizationMode_SynchronizationModeOneWaySafe ||
		mode == SynchronizationMode_SynchronizationModeOneWayReplica
	result := &RepositoryReconciliation{
		ancestor: ancestor,
		conflict: &RepositoryConflict{Ancestor: ancestor, Alpha: alpha, Beta: beta},
		oneWay:   oneWay,
	}

	// Create functions that produce the actions that move one repository to
	// the other's commit and branch, which only apply if the repository is
	// still in the state that it was scanned in.
	moveAlpha := func(policy RepositoryPolicy) {
		result.AlphaAction = &RepositoryAction{Old: alpha, Target: beta, Policy: policy}
	}
	moveBeta := func(policy RepositoryPolicy) {
		result.BetaAction = &RepositoryAction{Old: beta, Target: alpha, Policy: policy}
	}
	conflict := func() *RepositoryReconciliation {
		result.immediate = true
		return result
	}

	// Handle the cases where at least one repository has no commit (whether it
	// has no repository or an unborn one). There's nothing to agree on if
	// neither has one, while a repository without one gets the other's commit
	// unconditionally, since there's nothing that it could lose. One-way modes
	// never change alpha.
	alphaCommitted, betaCommitted := len(alpha.GetCommit()) > 0, len(beta.GetCommit()) > 0
	if !alphaCommitted || !betaCommitted {
		if alphaCommitted {
			moveBeta(RepositoryPolicy_Follow)
		} else if betaCommitted && !oneWay {
			moveAlpha(RepositoryPolicy_Follow)
		}
		return result
	}

	// If the repositories agree, then that's the new ancestor.
	if alpha.Equal(beta) {
		result.agreed = agreement(alpha)
		return result
	}

	// In the one-way-replica mode, beta always mirrors alpha once they've
	// agreed. A fast-forward wouldn't be enough, since beta can't be ahead of
	// alpha in a one-way mode without alpha's commit being a conflict, and
	// beta's working tree mirrors alpha's.
	replica := mode == SynchronizationMode_SynchronizationModeOneWayReplica
	if replica && ancestor != nil {
		moveBeta(RepositoryPolicy_Follow)
		return result
	}

	// If only one repository moved since they last agreed, then the other
	// follows it, except in one-way modes, where beta moving is either a
	// conflict or undone (above).
	if ancestor != nil && alpha.Equal(ancestor) {
		if oneWay {
			return conflict()
		}
		moveAlpha(RepositoryPolicy_Follow)
		return result
	} else if ancestor != nil && beta.Equal(ancestor) {
		moveBeta(RepositoryPolicy_Follow)
		return result
	}

	// Otherwise, both moved, or they never agreed. Repositories on different
	// branches conflict, unless alpha takes precedence, which requires that
	// they've agreed before.
	resolved := mode == SynchronizationMode_SynchronizationModeTwoWayResolved
	if alpha.Branch != beta.Branch {
		if resolved && ancestor != nil {
			moveBeta(RepositoryPolicy_Follow)
			return result
		}
		return conflict()
	}

	// Repositories on the same branch are fast-forwarded to whichever commit
	// contains the other, with alpha's commit replacing beta's if they've
	// diverged and alpha takes precedence (which again requires that they've
	// agreed before). One-way modes only fast-forward beta.
	betaPolicy := RepositoryPolicy_FastForward
	if resolved && ancestor != nil {
		betaPolicy = RepositoryPolicy_FastForwardOrFollow
	}
	moveBeta(betaPolicy)
	if !oneWay {
		moveAlpha(RepositoryPolicy_FastForward)
	}
	return result
}

// Staging returns the actions whose objects each endpoint needs (see
// Endpoint.StageRepository): those of its own action, if it has one. If the
// repositories conflict without any actions (such as because they're on
// different branches), then each endpoint that the session can change still
// gets the other's commit, without moving to it, so that the conflict can be
// resolved by resetting to it.
func (r *RepositoryReconciliation) Staging() (*RepositoryAction, *RepositoryAction) {
	if !r.immediate {
		return r.AlphaAction, r.BetaAction
	}
	alpha, beta := r.conflict.Alpha, r.conflict.Beta
	var alphaStaging *RepositoryAction
	if !r.oneWay {
		alphaStaging = &RepositoryAction{Old: alpha, Target: beta, Policy: RepositoryPolicy_FastForward}
	}
	return alphaStaging, &RepositoryAction{Old: beta, Target: alpha, Policy: RepositoryPolicy_FastForward}
}

// Complete determines the new ancestor and conflict once the reconciliation's
// actions have been applied, given their results (which are nil for actions
// that don't exist or weren't applied). If an action moved a repository, then
// both endpoints now have its target's commit and branch. Otherwise, if the
// commits have diverged (or, in one-way modes, if beta's commit contains
// alpha's), then the repositories conflict. Actions that failed leave the
// ancestor as it is, without a conflict, since their problems are reported
// separately, and the next synchronization cycle tries again.
func (r *RepositoryReconciliation) Complete(alpha, beta *RepositoryResult) (*Repository, *RepositoryConflict) {
	// Handle reconciliations without actions, which have no results.
	if r.AlphaAction == nil && r.BetaAction == nil {
		if r.agreed != nil {
			return r.agreed, nil
		} else if r.immediate {
			return r.ancestor, r.conflict
		}
		return r.ancestor, nil
	}

	// Ignore the results of actions that don't exist. A missing result has the
	// outcome of a failure.
	if r.AlphaAction == nil {
		alpha = nil
	}
	if r.BetaAction == nil {
		beta = nil
	}
	alphaOutcome, betaOutcome := alpha.GetOutcome(), beta.GetOutcome()

	// If exactly one repository moved, then both now agree on the other's
	// commit and branch. At most one can move, since each action targets the
	// other repository's original commit, and they can only both fast-forward
	// to each other's commit if they're the same, but if both somehow did,
	// then the next cycle determines what they agree on.
	alphaMoved := alphaOutcome == RepositoryOutcome_Moved
	betaMoved := betaOutcome == RepositoryOutcome_Moved
	if alphaMoved && betaMoved {
		return r.ancestor, nil
	} else if alphaMoved {
		return agreement(r.AlphaAction.Target), nil
	} else if betaMoved {
		return agreement(r.BetaAction.Target), nil
	}

	// Commits that have diverged conflict. In one-way modes, beta's commit
	// containing alpha's also conflicts, since alpha never takes it, while in
	// two-way modes, alpha fast-forwards to it.
	if alphaOutcome == RepositoryOutcome_Diverged || betaOutcome == RepositoryOutcome_Diverged ||
		(r.oneWay && betaOutcome == RepositoryOutcome_Ahead) {
		return r.ancestor, r.conflict
	}

	// Otherwise, an action failed, or a repository is ahead of the other,
	// which the other's action failed to fast-forward to.
	return r.ancestor, nil
}
