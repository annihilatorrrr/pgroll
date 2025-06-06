// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	_ "github.com/lib/pq"
	"sigs.k8s.io/yaml"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// Operation is an operation that can be applied to a schema
type Operation interface {
	// Start will apply the required changes to enable supporting the new schema
	// version in the database (through a view)
	// update the given views to expose the new schema version
	// Returns the table that requires backfilling, if any.
	Start(ctx context.Context, l Logger, conn db.DB, latestSchema string, s *schema.Schema) (*schema.Table, error)

	// Complete will update the database schema to match the current version
	// after calling Start.
	// This method should be called once the previous version is no longer used.
	Complete(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) error

	// Rollback will revert the changes made by Start. It is not possible to
	// rollback a completed migration.
	Rollback(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) error

	// Validate returns a descriptive error if the operation cannot be applied to the given schema.
	Validate(ctx context.Context, s *schema.Schema) error
}

// IsolatedOperation is an operation that cannot be executed with other operations
// in the same migration.
type IsolatedOperation interface {
	// IsIsolated defines where this operation is isolated when executed on start, cannot be executed
	// with other operations.
	IsIsolated() bool
}

// RequiresSchemaRefreshOperation is an operation that requires the resulting schema to be refreshed.
type RequiresSchemaRefreshOperation interface {
	// RequiresSchemaRefresh defines if this operation requires the resulting schema to be refreshed when
	// executed on start.
	RequiresSchemaRefresh()
}

type (
	Operations []Operation
	Migration  struct {
		Name string `json:"name,omitempty"`

		Operations Operations `json:"operations"`
	}
)

// Validate will check that the migration can be applied to the given schema
// returns a descriptive error if the migration is invalid
func (m *Migration) Validate(ctx context.Context, s *schema.Schema) error {
	for _, op := range m.Operations {
		if isolatedOp, ok := op.(IsolatedOperation); ok {
			if isolatedOp.IsIsolated() && len(m.Operations) > 1 {
				return InvalidMigrationError{Reason: fmt.Sprintf("operation %q cannot be executed with other operations", OperationName(op))}
			}
		}
	}

	for _, op := range m.Operations {
		err := op.Validate(ctx, s)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateVirtualSchema updates the in-memory schema representation with the changes
// made by the migration. No changes are made to the physical database.
func (m *Migration) UpdateVirtualSchema(ctx context.Context, s *schema.Schema) error {
	db := &db.FakeDB{}

	// Run `Start` on each operation using the fake DB. Updates will be made to
	// the in-memory schema `s` without touching the physical database.
	for _, op := range m.Operations {
		if _, err := op.Start(ctx, NewNoopLogger(), db, "", s); err != nil {
			return err
		}
	}
	return nil
}

// ContainsRawSQLOperation returns true if the migration contains a raw SQL operation
func (m *Migration) ContainsRawSQLOperation() bool {
	for _, op := range m.Operations {
		if _, ok := op.(*OpRawSQL); ok {
			return true
		}
	}
	return false
}

// WriteAsJSON writes the migration to the given writer in JSON format
func (m *Migration) WriteAsJSON(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	return encoder.Encode(m)
}

// WriteAsYAML writes the migration to the given writer in YAML format
func (m *Migration) WriteAsYAML(w io.Writer) error {
	yml, err := yaml.Marshal(m)
	if err != nil {
		return err
	}

	_, err = w.Write(yml)
	return err
}
