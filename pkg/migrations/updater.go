// SPDX-License-Identifier: Apache-2.0

package migrations

import "encoding/json"

type UpdaterFn func(operation map[string]any) (map[string]any, error)

// FileUpdater updates raw migration files if they contain any breaking
// changes that have proper updater functions registered.
type FileUpdater struct {
	updaterFns map[string][]UpdaterFn
}

func NewFileUpdater(updaters map[string][]UpdaterFn) *FileUpdater {
	return &FileUpdater{
		updaterFns: updaters,
	}
}

func (u *FileUpdater) Update(rawMigration *RawMigration) (*Migration, error) {
	var ops []map[string]any
	if err := json.Unmarshal(rawMigration.Operations, &ops); err != nil {
		return nil, err
	}
	var err error
	for _, op := range ops {
		for opName, fns := range u.updaterFns {
			// if the operation does not have registered updater function, do nothing
			if _, ok := op[opName]; !ok {
				continue
			}
			// run all registered updater functions on operation
			for _, fn := range fns {
				op, err = fn(op)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	rawMigration.Operations, err = json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	return ParseMigration(rawMigration)
}

// UpdateCreateIndexColumnsList transforms create_index's columns attribute from a list into a map
// columns: [name] -> columns: name: {}
// breaking change was released in v0.10.0
// PR: https://github.com/xataio/pgroll/pull/697
func UpdateCreateIndexColumnsList(op map[string]any) (map[string]any, error) {
	body, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	var createIndexOp struct {
		CreateIndex struct {
			Columns []string `json:"columns"`
		} `json:"create_index"`
	}

	// error is ignored here, because it can only happened if the create_index
	// operation does not contain the expected, outdated structure
	if err := json.Unmarshal(body, &createIndexOp); err == nil {
		if createIndexOper, ok := op["create_index"].(map[string]any); ok {
			delete(createIndexOper, "columns")
			columnsList := make(map[string]any, len(createIndexOp.CreateIndex.Columns))
			for _, col := range createIndexOp.CreateIndex.Columns {
				columnsList[col] = map[string]any{}
			}
			createIndexOper["columns"] = columnsList
		}
	}

	return op, nil
}
