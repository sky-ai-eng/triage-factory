package sqlite

// ParseDBDatetimeForTest exposes parseDBDatetime to the sqlite_test
// package so the parser-coverage test in factory_test.go can drive
// the helper directly. Files ending in _test.go in the production
// package are compiled only under `go test`, so this alias never
// reaches a build artifact — it's the standard Go pattern for
// surfacing an unexported helper to an external test package.
var ParseDBDatetimeForTest = parseDBDatetime

// PRCreatedWithoutDeliverableQueryForTest exposes the PR backstop read's
// statement so the query-plan test can EXPLAIN exactly what the store runs.
var PRCreatedWithoutDeliverableQueryForTest = prCreatedWithoutDeliverableQuery

// PresentExternalIDsQueryForTest exposes the review-id lookup's statement up
// to its IN list so the query-plan test can EXPLAIN what the store runs.
var PresentExternalIDsQueryForTest = presentExternalIDsQuery

// ActionObjectIDSQLForTest exposes the SQL twin of domain.ObjectID so the
// ledger conformance suite can hold the two to each other.
const ActionObjectIDSQLForTest = sqliteActionObjectIDSQL
