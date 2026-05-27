package sqlite

import (
	"fmt"
	"strings"
)

// FindParent runs the session-inference IN-clause query described in
// ARCHITECTURE § 5.2. Given a trace's strict-prefix canonical hashes
// (in descending-length order), it returns the longest-matching prior
// trace's (id, session_root_id, prefix_len). If no prior trace matches,
// ok is false.
//
// strictHashes MUST be ordered by length descending so the SQL's
// ORDER BY prefix_len DESC stably returns the longest match. The
// caller's responsibility — sqlite has no information about prefix
// semantics, only the hashes.
func (s *Store) FindParent(keyHash string, strictHashes []string) (parentID, sessionRootID string, prefixLen int, ok bool, err error) {
	if len(strictHashes) == 0 {
		return "", "", 0, false, nil
	}

	// Build IN-clause placeholders.
	placeholders := strings.Repeat("?,", len(strictHashes))
	placeholders = placeholders[:len(placeholders)-1] // strip trailing comma

	q := fmt.Sprintf(`
		SELECT id, session_root_id, prefix_len
		FROM traces
		WHERE key_hash = ?
		  AND prefix_canonical_hash IN (%s)
		ORDER BY prefix_len DESC, ts_start DESC
		LIMIT 1
	`, placeholders)

	args := make([]any, 0, len(strictHashes)+1)
	args = append(args, keyHash)
	for _, h := range strictHashes {
		args = append(args, h)
	}

	row := s.db.QueryRow(q, args...)
	var pid, root string
	var plen int
	if err := row.Scan(&pid, &root, &plen); err != nil {
		// sql.ErrNoRows is the expected "no parent found" case.
		if strings.Contains(err.Error(), "no rows") {
			return "", "", 0, false, nil
		}
		return "", "", 0, false, fmt.Errorf("find parent: %w", err)
	}
	return pid, root, plen, true, nil
}
