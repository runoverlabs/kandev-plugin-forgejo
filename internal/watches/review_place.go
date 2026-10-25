package watches

import (
	"context"
	"regexp"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// autoStartAction is the workflow step action that launches an agent on entry.
const autoStartAction = "auto_start_agent"

// launchActions are the on_enter actions that can run an agent when a task
// enters a step. A step carrying any of them is not a safe place for a fork's
// task.
var launchActions = map[string]struct{}{
	autoStartAction:                  {},
	"queue_run":                      {},
	"queue_run_for_each_participant": {},
	"run_code_review":                {},
}

// StartsAgent reports whether any of a step's on_enter action types can launch
// an agent.
func StartsAgent(actions []string) bool {
	for _, action := range actions {
		if _, launches := launchActions[action]; launches {
			return true
		}
	}
	return false
}

// safeRef accepts a branch name that is safe to hand to a checkout: no leading
// dash (an option), no "..", no control characters, no whitespace. A name that
// fails creates the task without a checkout rather than failing the watch.
var safeRef = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+@-]{0,199}$`)

func safeBranch(name string) bool {
	return safeRef.MatchString(name) && !strings.Contains(name, "..") &&
		!strings.HasSuffix(name, ".lock") && !strings.HasSuffix(name, "/")
}

// placement is where a pull request's task goes and whether it may start.
type placement struct {
	stepID     string
	startAgent bool
	checkout   bool
}

// steps resolves the watch's workflow steps once per run.
type steps struct {
	loaded bool
	byID   map[string][]string
	err    error
}

func (s *steps) autoStarts(ctx context.Context, host pluginsdk.Host, workflowID, stepID string) (starts, known bool) {
	if !s.loaded {
		s.loaded = true
		listed, err := host.Workflows().ListSteps(ctx, workflowID)
		if err != nil {
			s.err = err
			return false, false
		}
		s.byID = make(map[string][]string, len(listed))
		for _, step := range listed {
			s.byID[step.ID] = step.OnEnterActionTypes
		}
	}
	actions, found := s.byID[stepID]
	if !found {
		return false, false
	}
	return StartsAgent(actions), true
}

// place decides where a pull request's task lands.
//
// A fork's head is content its author controls, and a task that checks it out
// and starts an agent runs it with the operator's executor environment.
// GitHub's watches mark such a task so the host refuses an automatic start; a
// plugin cannot set that marker, so the protection here is a placement that
// cannot start an agent: no checkout, no start flag, and a step that does not
// start one on entry. If no such step exists the pull request is skipped.
func (p *Poller) place(ctx context.Context, host pluginsdk.Host, watch Watch, detail PullDetail, resolved *steps) (placement, bool) {
	if !detail.Fork {
		return placement{
			stepID:     watch.WorkflowStepID,
			startAgent: watch.StartAgent,
			checkout:   safeBranch(detail.HeadBranch),
		}, true
	}
	target := watch.WorkflowStepID
	if watch.ForkWorkflowStepID != "" {
		target = watch.ForkWorkflowStepID
	}
	// Checked on every run, not only at save time: the step can be edited later.
	starts, known := resolved.autoStarts(ctx, host, watch.WorkflowID, target)
	if !known || starts {
		return placement{}, false
	}
	return placement{stepID: target}, true
}
