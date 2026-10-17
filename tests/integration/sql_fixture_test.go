//go:build linux

package integration

import (
	"database/sql"

	"pwnmesh/internal/testutil/boardfixture"
)

type fixtureSQLTx = sql.Tx

var openFixtureStore = boardfixture.Open
var fixtureSQL = boardfixture.SQL
