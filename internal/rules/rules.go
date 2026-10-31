package rules

import "errors"

type Step struct {
	Rule      string   `json:"rule"`
	Approvers []string `json:"approvers"`
}

type Decision struct {
	StepIndex  int
	ApproverID string
	Action     string
}

func Normalize(rule string, approvers []string, steps []Step) ([]Step, error) {
	if rule == "sequence" {
		if len(steps) == 0 {
			return nil, errors.New("bad request")
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
		return nil, errors.New("bad request")
	}
	clean, err := checkStep(Step{Rule: rule, Approvers: approvers})
	if err != nil {
		return nil, err
	}
	return []Step{clean}, nil
}

func checkStep(step Step) (Step, error) {
	if step.Rule != "any" && step.Rule != "all" {
		return Step{}, errors.New("bad request")
	}
	if len(step.Approvers) == 0 {
		return Step{}, errors.New("bad request")
	}
	seen := map[string]bool{}
	for _, id := range step.Approvers {
		if seen[id] {
			return Step{}, errors.New("bad request")
		}
		seen[id] = true
	}
	return step, nil
}

func Evaluate(steps []Step, decisions []Decision) string {
	for i, step := range steps {
		mine := filter(decisions, i)
		for _, d := range mine {
			if d.Action == "reject" {
				return "rejected"
			}
		}
		if !passed(step, mine) {
			return "pending"
		}
	}
	return "approved"
}

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

func filter(decisions []Decision, step int) []Decision {
	var out []Decision
	for _, d := range decisions {
		if d.StepIndex == step {
			out = append(out, d)
		}
	}
	return out
}

func passed(step Step, decisions []Decision) bool {
	ok := map[string]bool{}
	for _, d := range decisions {
		if d.Action == "approve" {
			ok[d.ApproverID] = true
		}
	}
	if step.Rule == "any" {
		return len(ok) > 0
	}
	for _, id := range step.Approvers {
		if !ok[id] {
			return false
		}
	}
	return true
}
