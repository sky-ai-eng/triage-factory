package routing

// claimFailures tracks a worker's consecutive claim failures so a sustained
// database fault escalates loudly instead of only dripping a per-tick line.
// A transient blip clears in one tick; a persistent schema or connection
// fault would otherwise only ever repeat a uniform notice, so the threshold
// turns a sustained outage into an obviously different, periodically
// repeated signal, and a recovery line when it clears. The floor scan paces
// retries, so no extra backoff is needed — and adding one would only slow
// recovery once the database heals.
type claimFailures struct {
	// name is the worker as its log lines name it.
	name  string
	count int
}

// claimErrorEscalateThreshold is how many consecutive claim failures a
// worker tolerates before escalating from a one-line notice to a loud
// "stalled" warning.
const claimErrorEscalateThreshold = 5

// observe records one drain pass's claim result: nil clears the streak,
// logging a recovery when the streak had escalated.
func (c *claimFailures) observe(err error) {
	if err != nil {
		c.count++
		switch {
		case c.count == 1:
			routerLog.Warn(c.name+" claim failed, retrying on the next scan", "error", err)
		case c.count == claimErrorEscalateThreshold || c.count%claimErrorEscalateThreshold == 0:
			routerLog.Error(c.name+" stalled, not progressing", "claim_failures", c.count, "error", err)
		}
		return
	}
	if c.count >= claimErrorEscalateThreshold {
		routerLog.Info(c.name+" recovered", "claim_failures", c.count)
	}
	c.count = 0
}
