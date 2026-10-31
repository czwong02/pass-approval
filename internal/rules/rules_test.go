package rules

import "testing"


func TestAllNeedsBoth(t *testing.T) {
	steps := []Step{{Rule: "all", Approvers: []string{"a", "b"}}}
	if Evaluate(steps, []Decision{{0, "a", "approve"}}) != "pending" {
		t.Fatal("one vote")
	}
	if Evaluate(steps, []Decision{{0, "a", "approve"}, {0, "b", "approve"}}) != "approved" {
		t.Fatal("both")
	}
	if Open(steps, []Decision{{0, "a", "approve"}}) != 0 {
		t.Fatal("still step 0")
	}
}

func TestRejectVeto(t *testing.T) {
	steps := []Step{{Rule: "any", Approvers: []string{"a", "b"}}}
	if Evaluate(steps, []Decision{{0, "a", "reject"}}) != "rejected" {
		t.Fatal()
	}
}

func TestSequenceHoldsStep2(t *testing.T) {
	steps := []Step{{Rule: "any", Approvers: []string{"a"}}, {Rule: "any", Approvers: []string{"b"}}}
	if Open(steps, nil) != 0 {
		t.Fatal("step 1")
	}
	if Open(steps, []Decision{{0, "a", "approve"}}) != 1 {
		t.Fatal("step 2")
	}
}