package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// A selector is never deleted or reused, including when its question closes.
func (s *Store) humanSelector(ctx context.Context, tx *sql.Tx, featureID, requestID string) (int, error) {
	var existingFeature string
	var selector int
	err := tx.QueryRowContext(ctx, `SELECT feature_id,selector FROM human_request_selectors WHERE request_id=?`, requestID).Scan(&existingFeature, &selector)
	if err == nil {
		if existingFeature != featureID {
			return 0, errors.New("human selector feature mismatch")
		}
		return selector, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(selector),0)+1 FROM human_request_selectors WHERE feature_id=?`, featureID).Scan(&selector); err != nil {
		return 0, err
	}
	if selector <= 0 || selector > 1000000 {
		return 0, errors.New("human selector exhausted")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO human_request_selectors(feature_id,request_id,selector) VALUES(?,?,?)`, featureID, requestID, selector)
	return selector, err
}

// Existing questions are upgraded in publication order under the same SQLite
// transaction as schema bootstrap. Reopening a state cannot renumber them.
func (s *Store) backfillHumanSelectors(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,feature_id,data FROM slack_outbox WHERE id IN (SELECT request_id FROM human_projections) ORDER BY rowid`)
	if err != nil {
		return err
	}
	type oldQuestion struct {
		id, featureID string
		raw           []byte
	}
	var questions []oldQuestion
	for rows.Next() {
		var q oldQuestion
		if err = rows.Scan(&q.id, &q.featureID, &q.raw); err != nil {
			break
		}
		questions = append(questions, q)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, q := range questions {
		var d SlackDelivery
		if err = json.Unmarshal(q.raw, &d); err != nil {
			return err
		}
		if d.ID != q.id || d.FeatureID != q.featureID {
			return fmt.Errorf("invalid human question outbox %s", q.id)
		}
		selector, err := s.humanSelector(ctx, tx, q.featureID, q.id)
		if err != nil {
			return err
		}
		if d.ShortSelector != 0 && d.ShortSelector != selector {
			return fmt.Errorf("human question selector changed for %s", q.id)
		}
		if d.ShortSelector == 0 {
			d.ShortSelector = selector
			if _, err = tx.ExecContext(ctx, `UPDATE slack_outbox SET data=? WHERE id=?`, mustJSON(d), q.id); err != nil {
				return err
			}
		}
	}
	return nil
}
