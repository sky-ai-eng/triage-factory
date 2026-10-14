package sqlite

// ParseDBDatetimeForTest exposes parseDBDatetime to the sqlite_test
// package so the parser-coverage test in factory_test.go can drive
// the helper directly. Files ending in _test.go in the production
// package are compiled only under `go test`, so this alias never
// reaches a build artifact — it's the standard Go pattern for
// surfacing an unexported helper to an external test package.
var ParseDBDatetimeForTest = parseDBDatetime

// PRCreatedWithoutArtifactQueryForTest exposes the PR backstop read's
// statement so the query-plan test can EXPLAIN exactly what the store runs.
var PRCreatedWithoutArtifactQueryForTest = prCreatedWithoutArtifactQuery
