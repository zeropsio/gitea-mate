package pipeline

// WaitRunnerWork blocks until every runner build the pipeline is watching and
// every runner replacement it started has ended.
func (p *Pipeline) WaitRunnerWork() { p.runnerWork.Wait() }

// SweepWait is how long an import waits after a sweep that did not finish.
var SweepWait = sweepWait
