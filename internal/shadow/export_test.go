package shadow

import "github.com/jackc/pgx/v5/pgconn"

func tagOf(s string) pgconn.CommandTag { return pgconn.NewCommandTag(s) }
