package rules

import "errors"

// Step is one any/all group. A sequence is a list of these.
type Step struct {
	Rule      string   `json:"rule"`
	Approvers []string `json:"approvers"`
}

// Decision is one recorded vote. StepIndex is which step it belongs to.
type Decision struct {
	StepIndex  int
	ApproverID string
	Action     string
}

// Normalize turns a create body into the step list stored on the request.
// any and all become one step built from approvers.
// sequence must be a non-empty list of any/all steps.
// An empty approver list, a repeated approver, or an unknown rule is an error.
func Normalize(rule string, approvers []string, steps []Step) ([]Step, error) {
	// sequence: reject an empty list, then checkStep each entry.
	// any or all: checkStep one step made from rule and approvers.
	// anything else: return an error.
	if rule == "sequence" {
		if len(steps) == 0 {
			return nil, errors.New("sequence must have at least one step")
		}
		out := make([]Step, len(steps))
		for i, step := range steps {
			clean, err := checkStep(step)
			if err != nil {
				return nil, err
			}
			out[i] = clean
		}
		return out, nil
	}

	if rule != "any" && rule != "all" {
		return nil, errors.New("unknown rule")
	}
	if len(approvers) == 0 {
		return nil, errors.New("no approvers")
	}
	clean, err := checkStep(Step{Rule: rule, Approvers: approvers})
	if err != nil {
		return nil, err
	}
	return []Step{clean}, nil
}

// checkStep accepts only rule any or all, with at least one approver and no duplicates.
func checkStep(step Step) (Step, error) {
	if step.Rule != "any" && step.Rule != "all" {
		return Step{}, errors.New("unknown rule")
	}
	if len(step.Approvers) == 0 {
		return Step{}, errors.New("no approvers")
	}
	seen := map[string]bool{}
	for _, approver := range step.Approvers {
		if seen[approver] {
			return Step{}, errors.New("duplicate approver")
		}
		seen[approver] = true
	}
	return step, nil
}

// Evaluate walks the steps in order.
// A reject on the step in hand returns "rejected" immediately. Reject is a veto.
// If that step has not passed yet, return "pending".
// If every step has passed, return "approved".
func Evaluate(steps []Step, decisions []Decision) string {
	for i, step := range steps {
		mine := filter(decisions, i)
		for _, decision := range mine {
			if decision.Action == "reject" {
				return "rejected"
			}
		}
		if !passed(step, mine) {
			return "pending"
		}
	}

	return "approved"
}

// Open returns the index of the first step that has not passed.
// Return -1 when Evaluate is not "pending".
func Open(steps []Step, decisions []Decision) int {
	if Evaluate(steps, decisions) != "pending" {
		return -1
	}

	for i, step := range steps {
		if !passed(step, filter(decisions, i)) {
			return i
		}
	}

	return -1
}

// filter keeps decisions whose StepIndex matches step.
func filter(decisions []Decision, step int) []Decision {
	var out []Decision
	for _, decision := range decisions {
		if decision.StepIndex == step {
			out = append(out, decision)
		}
	}
	return out
}

// passed reports whether this step's approvals are enough.
// any: one approve is enough.
// all: every approver on the step must have approved.
// Rejects are not counted here. Evaluate handles the veto.
func passed(step Step, decisions []Decision) bool {
	ok := map[string]bool{}
	for _, decision := range decisions {
		if decision.Action == "approve" {
			ok[decision.ApproverID] = true
		}
	}
	if step.Rule == "any" {
		return len(ok) > 0
	}
	for _, approver := range step.Approvers {
		if !ok[approver] {
			return false
		}
	}
	return true
}
