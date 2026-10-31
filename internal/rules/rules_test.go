package rules

import "testing"

func TestAllNeedsBoth(t *testing.T) {
	steps := []Step{{Rule: "all", Approvers: []string{"a", "b"}}}
	if Evaluate(steps, []Decision{{StepIndex: 0, ApproverID: "a", Action: "approve"}}) != "pending" {
		t.Fatal("one vote")
	}
	if Evaluate(steps, []Decision{
		{StepIndex: 0, ApproverID: "a", Action: "approve"},
		{StepIndex: 0, ApproverID: "b", Action: "approve"},
	}) != "approved" {
		t.Fatal("both")
	}
	if Open(steps, []Decision{{StepIndex: 0, ApproverID: "a", Action: "approve"}}) != 0 {
		t.Fatal("still step 0")
	}
}

func TestRejectVeto(t *testing.T) {
	steps := []Step{{Rule: "any", Approvers: []string{"a", "b"}}}
	if Evaluate(steps, []Decision{{StepIndex: 0, ApproverID: "a", Action: "reject"}}) != "rejected" {
		t.Fatal()
	}
}

func TestSequenceHoldsStep2(t *testing.T) {
	steps := []Step{{Rule: "any", Approvers: []string{"a"}}, {Rule: "any", Approvers: []string{"b"}}}
	if Open(steps, nil) != 0 {
		t.Fatal("step 1")
	}
	if Open(steps, []Decision{{StepIndex: 0, ApproverID: "a", Action: "approve"}}) != 1 {
		t.Fatal("step 2")
	}
}
