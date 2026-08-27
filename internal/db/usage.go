package db

import (
	"context"
	"fmt"
)

// AddDownloads folds a batch of tallies into today's counts. Counts accumulate rather than
// replace, because the batch holds what happened since the last flush and not a total.
func (s *Store) AddDownloads(ctx context.Context, counts map[int64]int64) error {
	ids := make([]int64, 0, len(counts))
	tallies := make([]int64, 0, len(counts))
	for versionID, tally := range counts {
		ids = append(ids, versionID)
		tallies = append(tallies, tally)
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO downloads (version_id, day, count)
		SELECT id, current_date, tally FROM unnest($1::bigint[], $2::bigint[]) AS batch (id, tally)
		ON CONFLICT (version_id, day) DO UPDATE SET count = downloads.count + EXCLUDED.count`,
		ids, tallies)
	if err != nil {
		return fmt.Errorf("db: recording %d versions' downloads: %w", len(ids), err)
	}

	return nil
}

func (s *Store) TouchTokens(ctx context.Context, hashes [][]byte) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tokens SET last_used_at = now() WHERE hash = ANY($1)`, hashes)
	if err != nil {
		return fmt.Errorf("db: recording %d tokens' use: %w", len(hashes), err)
	}

	return nil
}
