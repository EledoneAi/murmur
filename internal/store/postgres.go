package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// postgres runs the SQL store on a pgx pool, which database/sql borrows its connections from.
type postgres struct {
	*sqlStore
	pool *pgxpool.Pool
}

// openPostgres connects to the database a postgres:// URL names. pgx reads the URL, pool settings included
// (pool_max_conns and the like).
func openPostgres(ctx context.Context, url string) (Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDBFromPool(pool)
	if err := migrate(ctx, db, "postgres"); err != nil {
		db.Close()
		pool.Close()
		return nil, err
	}
	return &postgres{&sqlStore{db: db}, pool}, nil
}

func (p *postgres) Close() error {
	err := p.sqlStore.Close()
	p.pool.Close()
	return err
}
