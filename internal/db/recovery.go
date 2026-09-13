package db

import (
	"context"
	"database/sql"
)

// ReplaceRecoveryCodes swaps a user's whole set of 2FA recovery codes.
func (d *DB) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM totp_recovery_codes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO totp_recovery_codes (user_id, code_hash) VALUES (?,?)`,
				userID, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// RedeemRecoveryCode consumes a code, reporting whether it existed. Deleting
// rather than flagging means a stolen database cannot reveal which codes the
// user has already spent.
func (d *DB) RedeemRecoveryCode(ctx context.Context, userID int64, codeHash string) (bool, error) {
	res, err := d.ExecContext(ctx,
		`DELETE FROM totp_recovery_codes WHERE user_id = ? AND code_hash = ?`, userID, codeHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountRecoveryCodes reports how many unused codes remain.
func (d *DB) CountRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM totp_recovery_codes WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}
