// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var _ Operation = (*OpCreateConstraint)(nil)

func (o *OpCreateConstraint) Start(ctx context.Context, l Logger, conn db.DB, latestSchema string, s *schema.Schema) (*schema.Table, error) {
	l.LogOperationStart(o)

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	columns := make([]*schema.Column, len(o.Columns))
	for i, colName := range o.Columns {
		columns[i] = table.GetColumn(colName)
		if columns[i] == nil {
			return nil, ColumnDoesNotExistError{Table: o.Table, Name: colName}
		}
	}

	// Duplicate each column using its final name after migration completion
	d := NewColumnDuplicator(conn, table, columns...)
	for _, colName := range o.Columns {
		d = d.WithName(table.GetColumn(colName).Name, TemporaryName(colName))
	}
	if err := d.Duplicate(ctx); err != nil {
		return nil, fmt.Errorf("failed to duplicate columns for new constraint: %w", err)
	}

	// Setup triggers
	for _, colName := range o.Columns {
		upSQL := o.Up[colName]
		err := NewCreateTriggerAction(conn,
			triggerConfig{
				Name:           TriggerName(o.Table, colName),
				Direction:      TriggerDirectionUp,
				Columns:        table.Columns,
				SchemaName:     s.Name,
				LatestSchema:   latestSchema,
				TableName:      table.Name,
				PhysicalColumn: TemporaryName(colName),
				SQL:            upSQL,
			},
		).Execute(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create up trigger: %w", err)
		}

		// Add the new column to the internal schema representation. This is done
		// here, before creation of the down trigger, so that the trigger can declare
		// a variable for the new column. Save the old column name for use as the
		// physical column name in the down trigger first.
		oldPhysicalColumn := table.GetColumn(colName).Name
		table.AddColumn(colName, &schema.Column{
			Name: TemporaryName(colName),
		})

		downSQL := o.Down[colName]
		err = NewCreateTriggerAction(conn,
			triggerConfig{
				Name:           TriggerName(o.Table, TemporaryName(colName)),
				Direction:      TriggerDirectionDown,
				Columns:        table.Columns,
				LatestSchema:   latestSchema,
				SchemaName:     s.Name,
				TableName:      table.Name,
				PhysicalColumn: oldPhysicalColumn,
				SQL:            downSQL,
			},
		).Execute(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create down trigger: %w", err)
		}
	}

	switch o.Type {
	case OpCreateConstraintTypeUnique, OpCreateConstraintTypePrimaryKey:
		return table, NewCreateUniqueIndexConcurrentlyAction(conn, s.Name, o.Name, table.Name, temporaryNames(o.Columns)...).Execute(ctx)
	case OpCreateConstraintTypeCheck:
		return table, o.addCheckConstraint(ctx, conn, table.Name)
	case OpCreateConstraintTypeForeignKey:
		return table, o.addForeignKeyConstraint(ctx, conn, table)
	}

	return table, nil
}

func (o *OpCreateConstraint) Complete(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) error {
	l.LogOperationComplete(o)

	switch o.Type {
	case OpCreateConstraintTypeUnique:
		uniqueOp := &OpSetUnique{
			Table: o.Table,
			Name:  o.Name,
		}
		err := uniqueOp.Complete(ctx, l, conn, s)
		if err != nil {
			return err
		}
	case OpCreateConstraintTypeCheck:
		checkOp := &OpSetCheckConstraint{
			Table: o.Table,
			Check: CheckConstraint{
				Name: o.Name,
			},
		}
		err := checkOp.Complete(ctx, l, conn, s)
		if err != nil {
			return err
		}
	case OpCreateConstraintTypeForeignKey:
		fkOp := &OpSetForeignKey{
			Table: o.Table,
			References: ForeignKeyReference{
				Name: o.Name,
			},
		}
		err := fkOp.Complete(ctx, l, conn, s)
		if err != nil {
			return err
		}
	case OpCreateConstraintTypePrimaryKey:
		_, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY USING INDEX %s",
			pq.QuoteIdentifier(o.Table),
			pq.QuoteIdentifier(o.Name),
		))
		if err != nil {
			return err
		}
	}

	for _, col := range o.Columns {
		if err := alterSequenceOwnerToDuplicatedColumn(ctx, conn, o.Table, col); err != nil {
			return err
		}
	}

	removeOldColumns := NewDropColumnAction(conn, o.Table, o.Columns...)
	err := removeOldColumns.Execute(ctx)
	if err != nil {
		return err
	}

	// rename new columns to old name
	table := s.GetTable(o.Table)
	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}
	for _, col := range o.Columns {
		column := table.GetColumn(col)
		if column == nil {
			return ColumnDoesNotExistError{Table: o.Table, Name: col}
		}
		if err := RenameDuplicatedColumn(ctx, conn, table, column); err != nil {
			return err
		}
	}

	if err := o.removeTriggers(ctx, conn); err != nil {
		return err
	}

	removeBackfillColumn := NewDropColumnAction(conn, o.Table, backfill.CNeedsBackfillColumn)
	err = removeBackfillColumn.Execute(ctx)

	return err
}

func (o *OpCreateConstraint) Rollback(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) error {
	l.LogOperationRollback(o)

	table := s.GetTable(o.Table)
	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}

	removeDuplicatedColumns := NewDropColumnAction(conn, table.Name, temporaryNames(o.Columns)...)
	err := removeDuplicatedColumns.Execute(ctx)
	if err != nil {
		return err
	}

	if err := o.removeTriggers(ctx, conn); err != nil {
		return err
	}

	removeBackfillColumn := NewDropColumnAction(conn, table.Name, backfill.CNeedsBackfillColumn)
	err = removeBackfillColumn.Execute(ctx)

	return err
}

func (o *OpCreateConstraint) removeTriggers(ctx context.Context, conn db.DB) error {
	dropFuncs := make([]string, 0, len(o.Columns)*2)
	for _, column := range o.Columns {
		dropFuncs = append(dropFuncs, TriggerFunctionName(o.Table, column))
		dropFuncs = append(dropFuncs, TriggerFunctionName(o.Table, TemporaryName(column)))
	}
	return NewDropFunctionAction(conn, dropFuncs...).Execute(ctx)
}

func (o *OpCreateConstraint) Validate(ctx context.Context, s *schema.Schema) error {
	table := s.GetTable(o.Table)
	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}

	if err := ValidateIdentifierLength(o.Name); err != nil {
		return err
	}

	if table.ConstraintExists(o.Name) {
		return ConstraintAlreadyExistsError{
			Table:      o.Table,
			Constraint: o.Name,
		}
	}

	for _, col := range o.Columns {
		if table.GetColumn(col) == nil {
			return ColumnDoesNotExistError{
				Table: o.Table,
				Name:  col,
			}
		}
		if _, ok := o.Up[col]; !ok {
			return ColumnMigrationMissingError{
				Table: o.Table,
				Name:  col,
			}
		}
		if _, ok := o.Down[col]; !ok {
			return ColumnMigrationMissingError{
				Table: o.Table,
				Name:  col,
			}
		}
	}

	switch o.Type {
	case OpCreateConstraintTypeUnique:
		if len(o.Columns) == 0 {
			return FieldRequiredError{Name: "columns"}
		}
	case OpCreateConstraintTypeCheck:
		if o.Check == nil || *o.Check == "" {
			return FieldRequiredError{Name: "check"}
		}
	case OpCreateConstraintTypeForeignKey:
		if o.References == nil {
			return FieldRequiredError{Name: "references"}
		}
		table := s.GetTable(o.References.Table)
		if table == nil {
			return TableDoesNotExistError{Name: o.References.Table}
		}
		for _, col := range o.References.Columns {
			if table.GetColumn(col) == nil {
				return ColumnDoesNotExistError{
					Table: o.References.Table,
					Name:  col,
				}
			}
		}
	}

	return nil
}

func (o *OpCreateConstraint) addCheckConstraint(ctx context.Context, conn db.DB, tableName string) error {
	sql := fmt.Sprintf("ALTER TABLE %s ADD ", pq.QuoteIdentifier(tableName))

	writer := &ConstraintSQLWriter{
		Name:           o.Name,
		SkipValidation: true,
	}
	sql += writer.WriteCheck(rewriteCheckExpression(*o.Check, o.Columns...), o.NoInherit)
	_, err := conn.ExecContext(ctx, sql)
	return err
}

func (o *OpCreateConstraint) addForeignKeyConstraint(ctx context.Context, conn db.DB, table *schema.Table) error {
	sql := fmt.Sprintf("ALTER TABLE %s ADD ", pq.QuoteIdentifier(table.Name))

	writer := &ConstraintSQLWriter{
		Name:           o.Name,
		Columns:        temporaryNames(o.Columns),
		SkipValidation: true,
	}
	sql += writer.WriteForeignKey(
		o.References.Table,
		o.References.Columns,
		o.References.OnDelete,
		o.References.OnUpdate,
		o.References.OnDeleteSetColumns,
		o.References.MatchType,
	)

	_, err := conn.ExecContext(ctx, sql)

	return err
}

func temporaryNames(columns []string) []string {
	names := make([]string, len(columns))
	for i, col := range columns {
		names[i] = TemporaryName(col)
	}
	return names
}
