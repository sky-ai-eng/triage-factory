package workitem

// IndexDDL is the exact DDL for the indexes an adopting table must carry. The
// package owns the statements because the claim and guard shapes are the
// package's; the table owns creating them, since the package ships no
// migration.
//
// Both dialects accept these statements verbatim — SQLite has had partial
// indexes since 3.8 — so the text does not vary by dialect, only by the
// uniqueness mode the kind declared.
func IndexDDL(k Kind) []string {
	t := k.Table
	out := []string{
		// Claim's first arm: the ripe-ready scan, in FIFO order.
		"CREATE INDEX IF NOT EXISTS " + indexName(t, "ready_next") +
			" ON " + t + " (next_attempt_at, id) WHERE status = 'ready'",
		// Claim's second arm: a cancellation request must not wait for a
		// deferred row's retry time, so it gets its own entry point.
		"CREATE INDEX IF NOT EXISTS " + indexName(t, "ready_cancel") +
			" ON " + t + " (id) WHERE status = 'ready' AND cancel_requested_at IS NOT NULL",
		// Claim's third arm: reclaiming expired leases.
		"CREATE INDEX IF NOT EXISTS " + indexName(t, "leased_expiry") +
			" ON " + t + " (lease_expires_at) WHERE status = 'leased'",
	}
	switch k.Unique {
	case UniqueForever:
		out = append(out, "CREATE UNIQUE INDEX IF NOT EXISTS "+uniqueIndexName(t)+
			" ON "+t+" (org_id, unique_key) WHERE unique_key IS NOT NULL")
	case UniqueWhileUnsettled:
		out = append(out, "CREATE UNIQUE INDEX IF NOT EXISTS "+uniqueIndexName(t)+
			" ON "+t+" (org_id, unique_key) WHERE unique_key IS NOT NULL AND status IN ("+unsettledStatusList+")")
	}
	return out
}

// unsettledStatusList is the statuses that still reserve a unique key under
// UniqueWhileUnsettled. Parked is among them: parked work holds its key until
// an operator redrives or supersedes it.
const unsettledStatusList = "'ready','leased','parked'"

// IndexNames is the names IndexDDL creates, in the same order. A conformance
// suite asserts presence by name, and an adopting table's migration needs them
// to drop or rename in step with the DDL above.
func IndexNames(k Kind) []string {
	out := []string{
		indexName(k.Table, "ready_next"),
		indexName(k.Table, "ready_cancel"),
		indexName(k.Table, "leased_expiry"),
	}
	if k.Unique != UniqueNone {
		out = append(out, uniqueIndexName(k.Table))
	}
	return out
}

func indexName(table, suffix string) string { return "idx_" + table + "_" + suffix }
func uniqueIndexName(table string) string   { return "uq_" + table + "_unique_key" }
