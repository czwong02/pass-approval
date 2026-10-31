export type Step = { rule: "any" | "all"; approvers: string[] }

export type DecisionRow = {
  stepIndex: number
  approverId: string
  action: "approve" | "reject"
}

export function normalizeSteps(rule: string, approvers: string[] | undefined, steps: Step[] | undefined): Step[] {
  if (rule === "sequence") {
    if (!steps?.length) throw new Error("bad request")
    return steps.map(checkStep)
  }
  if (rule !== "any" && rule !== "all") throw new Error("bad request")
  if (!approvers?.length) throw new Error("bad request")
  return [checkStep({ rule, approvers })]
}

function checkStep(step: Step): Step {
  if (step.rule !== "any" && step.rule !== "all") throw new Error("bad request")
  if (!step.approvers?.length) throw new Error("bad request")
  if (new Set(step.approvers).size !== step.approvers.length) throw new Error("bad request")
  return { rule: step.rule, approvers: step.approvers }
}

export function evaluate(steps: Step[], decisions: DecisionRow[]): "pending" | "approved" | "rejected" {
  for (let i = 0; i < steps.length; i++) {
    const mine = decisions.filter((decision) => decision.stepIndex === i)
    if (mine.some((decision) => decision.action === "reject")) return "rejected"
    if (!stepPassed(steps[i], mine)) return "pending"
  }
  return "approved"
}

export function openStepIndex(steps: Step[], decisions: DecisionRow[]): number | null {
  if (evaluate(steps, decisions) !== "pending") return null
  for (let i = 0; i < steps.length; i++) {
    const mine = decisions.filter((decision) => decision.stepIndex === i)
    if (!stepPassed(steps[i], mine)) return i
  }
  return null
}

function stepPassed(step: Step, decisions: DecisionRow[]): boolean {
  const approved = new Set(
    decisions.filter((decision) => decision.action === "approve").map((decision) => decision.approverId),
  )
  if (step.rule === "any") return approved.size > 0
  return step.approvers.every((id) => approved.has(id))
}