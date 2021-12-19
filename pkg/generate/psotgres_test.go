package generate

import (
	"testing"

	"github.com/schemahero/schemahero/pkg/database/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	trueValue = true
)

func Test_generatePostgres(t *testing.T) {
	tests := []struct {
		name         string
		driver       string
		dbName       string
		table        types.Table
		primaryKey   []string
		foreignKeys  []*types.ForeignKey
		indexes      []*types.Index
		columns      []*types.Column
		expectedYAML string
	}{
		{
			name:   "pg 1 col",
			driver: "postgres",
			dbName: "db",
			table: types.Table{
				Name: "simple",
			},
			primaryKey:  []string{"one"},
			foreignKeys: []*types.ForeignKey{},
			indexes:     []*types.Index{},
			columns: []*types.Column{
				{
					Name:     "id",
					DataType: "integer",
				},
			},
			expectedYAML: `apiVersion: schemas.schemahero.io/v1alpha4
kind: Table
metadata:
  name: simple
spec:
  database: db
  name: simple
  schema:
    postgres:
      primaryKey:
      - one
      columns:
      - name: id
        type: integer
`,
		},
		{
			name:   "pg foreign key",
			driver: "postgres",
			dbName: "db",
			table: types.Table{
				Name: "withfk",
			},
			primaryKey: []string{"pk"},
			foreignKeys: []*types.ForeignKey{
				{
					ChildColumns:  []string{"cc"},
					ParentTable:   "p",
					ParentColumns: []string{"pc"},
					Name:          "fk_pc_cc",
				},
			},
			indexes: []*types.Index{},
			columns: []*types.Column{
				{
					Name:     "pk",
					DataType: "integer",
				},
				{
					Name:     "cc",
					DataType: "integer",
				},
			},
			expectedYAML: `apiVersion: schemas.schemahero.io/v1alpha4
kind: Table
metadata:
  name: withfk
spec:
  database: db
  name: withfk
  schema:
    postgres:
      primaryKey:
      - pk
      foreignKeys:
      - columns:
        - cc
        references:
          table: p
          columns:
          - pc
        name: fk_pc_cc
      columns:
      - name: pk
        type: integer
      - name: cc
        type: integer
`,
		},
		{
			name:   "generating with index",
			driver: "postgres",
			dbName: "db",
			table: types.Table{
				Name: "simple",
			},
			primaryKey:  []string{"id"},
			foreignKeys: []*types.ForeignKey{},
			indexes: []*types.Index{
				{
					Columns:  []string{"other"},
					Name:     "idx_simple_other",
					IsUnique: true,
				},
			},
			columns: []*types.Column{
				{
					Name:     "id",
					DataType: "integer",
				},
				{
					Name:     "other",
					DataType: "varchar (255)",
					Constraints: &types.ColumnConstraints{
						NotNull: &trueValue,
					},
				},
			},
			expectedYAML: `apiVersion: schemas.schemahero.io/v1alpha4
kind: Table
metadata:
  name: simple
spec:
  database: db
  name: simple
  schema:
    postgres:
      primaryKey:
      - id
      indexes:
      - columns:
        - other
        name: idx_simple_other
        isUnique: true
      columns:
      - name: id
        type: integer
      - name: other
        type: varchar (255)
        constraints:
          notNull: true
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := require.New(t)
			actual, err := generatePostgresqlTableYAML(test.dbName, &test.table, test.primaryKey, test.foreignKeys, test.indexes, test.columns)
			req.NoError(err)
			assert.Equal(t, test.expectedYAML, actual)
		})
	}
}
