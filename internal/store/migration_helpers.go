package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mbianchidev/porto/internal/dataops"
)

func (s *Store) ReserveMigrationTemporary(ctx context.Context, resource dataops.SourceTemporary) error {
	if resource.Context == "" || resource.Endpoint == "" || resource.Owner == "" || resource.Name == "" ||
		resource.Kind != "image" && resource.Kind != "container" {
		return errors.New("invalid source migration helper reservation")
	}
	document, err := json.Marshal(resource)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO migration_source_temporaries(owner,kind,resource,reserved_at)
VALUES(?,?,?,?) ON CONFLICT(owner,kind) DO UPDATE SET resource=excluded.resource`,
		resource.Owner, resource.Kind, string(document), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ClearMigrationTemporary(ctx context.Context, resource dataops.SourceTemporary) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM migration_source_temporaries WHERE owner=? AND kind=?`, resource.Owner, resource.Kind)
	return err
}

func (s *Store) MigrationTemporaries(ctx context.Context) ([]dataops.SourceTemporary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource FROM migration_source_temporaries ORDER BY CASE kind WHEN 'container' THEN 0 ELSE 1 END,reserved_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]dataops.SourceTemporary, 0)
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var resource dataops.SourceTemporary
		if err := json.Unmarshal([]byte(document), &resource); err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, rows.Err()
}
