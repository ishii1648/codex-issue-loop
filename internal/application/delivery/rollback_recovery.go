package delivery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

func (c Controller) RetryRollback(ctx context.Context, expectedBackup string) (Report, error) {
	paths := RuntimePaths(c.Layout.Root)
	if err := paths.Ensure(); err != nil {
		return Report{}, err
	}
	cfg, err := LoadConfig(c.ConfigPath)
	if err != nil {
		return Report{}, err
	}
	lock, err := AcquireLock(paths.Lock)
	if err != nil {
		return Report{}, err
	}
	defer lock.Close()
	tx, err := LoadTransaction(paths.Transaction)
	if err != nil {
		return Report{}, err
	}
	reportError := func(cause error) (Report, error) {
		return c.reportFrom(paths, cfg, tx, tx.Drain), cause
	}
	if !transactionActive(tx) || tx.LastResult != "rollback_failed" {
		return reportError(errors.New("delivery rollback retry requires an active rollback_failed transaction"))
	}
	if expectedBackup == "" || filepath.Clean(expectedBackup) != filepath.Clean(tx.BackupPath) {
		return reportError(fmt.Errorf("delivery rollback retry backup does not match retained transaction backup %s", tx.BackupPath))
	}
	fence, err := LoadMaintenance(paths.Maintenance)
	if err != nil {
		return reportError(err)
	}
	if fence.Generation != tx.MaintenanceGeneration || fence.Desired != tx.Desired {
		return reportError(errors.New("delivery maintenance fence does not match the retained transaction"))
	}
	installed, err := readInstalled(filepath.Join(c.Layout.Root, "install.json"))
	if err != nil {
		return reportError(err)
	}
	current := installed.ref()
	if current != tx.Previous && current != tx.Desired {
		return reportError(fmt.Errorf("installed version %s@%s matches neither retained previous nor desired version", current.Version, current.Commit))
	}
	originalReason := tx.Reason
	if current == tx.Desired {
		report, rollbackErr := c.rollback(ctx, paths, cfg, &tx, tx.Drain, errors.New(originalReason))
		if report.Result == "rolled_back" {
			return report, nil
		}
		return report, rollbackErr
	}
	tx.LastResult = "rolling_back"
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	if err := c.health(ctx, tx.LoadedRepositories); err != nil {
		tx.LastResult = "rollback_failed"
		tx.Reason = fmt.Sprintf("%s; rollback health retry failed: %v; keep maintenance fence and inspect backup %s", originalReason, err, tx.BackupPath)
		_ = SaveTransaction(paths.Transaction, tx)
		return c.reportFrom(paths, cfg, tx, tx.Drain), errors.New(tx.Reason)
	}
	tx.LastResult = "rolled_back"
	tx.Reason = originalReason
	tx.Current = tx.Previous
	tx.Phase = PhaseVerified
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	if err := c.clearFence(paths); err != nil {
		return Report{}, err
	}
	return c.reportFrom(paths, cfg, tx, tx.Drain), nil
}
