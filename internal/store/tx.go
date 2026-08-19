package store

import "github.com/jackc/pgx/v5"

// pgxTx is the transaction interface used by store methods.
type pgxTx = pgx.Tx
