package db

import (
	"context"
	"errors"
	"os"

	"github.com/jackc/pgx/v5"
)

// Open reads DATABASE_URL and returns one connection.
// The Neon pooler is transaction-mode PgBouncer, so a prepared statement
// does not survive the next query. Set DefaultQueryExecMode to the simple protocol
// on the parsed config before connecting.
func Open(ctx context.Context) (*pgx.Conn, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return pgx.ConnectConfig(ctx, config)
}
