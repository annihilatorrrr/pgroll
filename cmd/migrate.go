// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/backfill"
)

func migrateCmd() *cobra.Command {
	var complete bool
	var batchSize int
	var batchDelay time.Duration

	migrateCmd := &cobra.Command{
		Use:       "migrate <directory>",
		Short:     "Apply outstanding migrations from a directory to a database",
		Example:   "migrate ./migrations",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"directory"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			migrationsDir := args[0]

			m, err := NewRoll(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			// Ensure that pgroll is initialized
			ok, err := m.State().IsInitialized(cmd.Context())
			if err != nil {
				return err
			}
			if !ok {
				return errPGRollNotInitialized
			}

			latestVersion, err := m.State().LatestVersion(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("unable to determine latest version: %w", err)
			}

			active, err := m.State().IsActiveMigrationPeriod(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("unable to determine active migration period: %w", err)
			}
			if active {
				fmt.Printf("migration %q is active\n", *latestVersion)
				return nil
			}

			info, err := os.Stat(migrationsDir)
			if err != nil {
				return fmt.Errorf("failed to stat directory: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("migrations directory %q is not a directory", migrationsDir)
			}

			migs, err := m.UnappliedMigrations(ctx, os.DirFS(migrationsDir))
			if err != nil {
				return fmt.Errorf("failed to get migrations to apply: %w", err)
			}

			if len(migs) == 0 {
				fmt.Println("database is up to date; no migrations to apply")
				return nil
			}

			backfillConfig := backfill.NewConfig(
				backfill.WithBatchSize(batchSize),
				backfill.WithBatchDelay(batchDelay),
			)

			// Run all migrations after the latest version up to the final migration,
			// completing each one.
			for _, mig := range migs[:len(migs)-1] {
				if err := runMigration(ctx, m, mig, true, backfillConfig); err != nil {
					return fmt.Errorf("failed to run migration file %q: %w", mig.Name, err)
				}
			}

			// Run the final migration, completing it only if requested.
			return runMigration(ctx, m, migs[len(migs)-1], complete, backfillConfig)
		},
	}

	migrateCmd.Flags().IntVar(&batchSize, "backfill-batch-size", backfill.DefaultBatchSize, "Number of rows backfilled in each batch")
	migrateCmd.Flags().DurationVar(&batchDelay, "backfill-batch-delay", backfill.DefaultDelay, "Duration of delay between batch backfills (eg. 1s, 1000ms)")
	migrateCmd.Flags().BoolVarP(&complete, "complete", "c", false, "complete the final migration rather than leaving it active")

	return migrateCmd
}
