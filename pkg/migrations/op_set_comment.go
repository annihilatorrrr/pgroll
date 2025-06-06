// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// OpSetComment is a operation that sets a comment on a object.
type OpSetComment struct {
	Table   string  `json:"table"`
	Column  string  `json:"column"`
	Comment *string `json:"comment"`
	Up      string  `json:"up"`
	Down    string  `json:"down"`
}

var _ Operation = (*OpSetComment)(nil)

func (o *OpSetComment) Start(ctx context.Context, l Logger, conn db.DB, latestSchema string, s *schema.Schema) (*schema.Table, error) {
	l.LogOperationStart(o)

	tbl := s.GetTable(o.Table)
	if tbl == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	return tbl, NewCommentColumnAction(conn, o.Table, TemporaryName(o.Column), o.Comment).Execute(ctx)
}

func (o *OpSetComment) Complete(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) error {
	l.LogOperationComplete(o)

	return NewCommentColumnAction(conn, o.Table, o.Column, o.Comment).Execute(ctx)
}

func (o *OpSetComment) Rollback(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) error {
	l.LogOperationRollback(o)

	return nil
}

func (o *OpSetComment) Validate(ctx context.Context, s *schema.Schema) error {
	return nil
}
