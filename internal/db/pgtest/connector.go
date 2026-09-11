package pgtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

// swappable is a *sql.DB whose server can be replaced without closing
// the handle. It dials through a driver.Connector that reads its target
// at dial time, so pointAt is enough to make the same *sql.DB talk to a
// different Postgres.
//
// That the handle's identity survives is the whole point. A conformance
// suite builds its store bundle over h.AdminDB once, above the
// per-subtest factory that hands the bundle's stores out, so the
// pointer outlives any single subtest. A harness that answered a
// container death by opening replacement pools would leave that bundle
// holding a closed one, and every remaining subtest failing on `sql:
// database is closed` — the same cascade reviving exists to prevent,
// one layer down.
type swappable struct {
	db *sql.DB

	mu sync.RWMutex
	to driver.Connector
}

// defaultMaxIdleConns mirrors database/sql's own default idle cap. The
// pool runs on it until pointAt has to name it explicitly to restore it
// after a flush.
const defaultMaxIdleConns = 2

// newSwappable returns a pool with no server yet; pointAt names one.
// Queries before that fail rather than block, which is what a harness
// whose bring-up never completed should do.
func newSwappable() *swappable {
	s := &swappable{}
	s.db = sql.OpenDB(s)
	return s
}

// Connect implements driver.Connector.
func (s *swappable) Connect(ctx context.Context) (driver.Conn, error) {
	s.mu.RLock()
	to := s.to
	s.mu.RUnlock()
	if to == nil {
		return nil, errors.New("pgtest: pool has no server — the harness never came up")
	}
	return to.Connect(ctx)
}

// Driver implements driver.Connector.
func (s *swappable) Driver() driver.Driver { return stdlib.GetDefaultDriver() }

// pointAt aims the pool at dsn and discards every connection it pooled
// against the previous one. Dropping the idle set is what makes the
// swap take effect on the very next query: database/sql does shed a
// dead connection on its own — pgx reports one as driver.ErrBadConn,
// which it retries twice before forcing a fresh dial — but that
// recovery runs per query and starts from connections already known to
// be dead. Setting the idle cap to zero closes them; the second call
// restores the cap.
func (s *swappable) pointAt(dsn string) error {
	drv, ok := s.Driver().(driver.DriverContext)
	if !ok {
		return errors.New("pgtest: pgx driver does not implement driver.DriverContext")
	}
	to, err := drv.OpenConnector(dsn)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.to = to
	s.mu.Unlock()
	s.db.SetMaxIdleConns(0)
	s.db.SetMaxIdleConns(defaultMaxIdleConns)
	return nil
}
