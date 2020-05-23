package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
	"github.com/pkg/errors"
	schemasv1alpha4 "github.com/schemahero/schemahero/pkg/apis/schemas/v1alpha4"
	"github.com/schemahero/schemahero/pkg/database/types"
)

func PlanPostgresTable(uri string, tableName string, postgresTableSchema *schemasv1alpha4.SQLTableSchema) ([]string, error) {
	p, err := Connect(uri)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to postgres")
	}
	defer p.db.Close()

	// determine if the table exists
	query := `select count(1) from information_schema.tables where table_name = $1`
	row := p.db.QueryRow(query, tableName)
	tableExists := 0
	if err := row.Scan(&tableExists); err != nil {
		return nil, errors.Wrap(err, "failed to scan")
	}

	if tableExists == 0 && postgresTableSchema.IsDeleted {
		return []string{}, nil
	} else if tableExists > 0 && postgresTableSchema.IsDeleted {
		return []string{
			fmt.Sprintf(`drop table %s`, pq.QuoteIdentifier(tableName)),
		}, nil
	}

	if tableExists == 0 {
		// shortcut to just create it
		query, err := CreateTableStatement(tableName, postgresTableSchema)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create table statement")
		}

		return []string{query}, nil
	}

	statements := []string{}

	// table needs to be altered?
	columnStatements, err := buildColumnStatements(p, tableName, postgresTableSchema)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build column statement")
	}
	statements = append(statements, columnStatements...)

	// primary key changes
	primaryKeyStatements, err := buildPrimaryKeyStatements(p, tableName, postgresTableSchema)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build primary key statements")
	}
	statements = append(statements, primaryKeyStatements...)

	// foreign key changes
	foreignKeyStatements, err := buildForeignKeyStatements(p, tableName, postgresTableSchema)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build foreign key statements")
	}
	statements = append(statements, foreignKeyStatements...)

	// index changes
	indexStatements, err := buildIndexStatements(p, tableName, postgresTableSchema)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build index statements")
	}
	statements = append(statements, indexStatements...)

	return statements, nil
}

func DeployPostgresStatements(uri string, statements []string) error {
	p, err := Connect(uri)
	if err != nil {
		return err
	}
	defer p.db.Close()

	// execute
	if err := executeStatements(p, statements); err != nil {
		return err
	}

	return nil
}

func executeStatements(p *PostgresConnection, statements []string) error {
	for _, statement := range statements {
		if statement == "" {
			continue
		}
		fmt.Printf("Executing query %q\n", statement)
		if _, err := p.db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}

	return nil
}

func buildColumnStatements(p *PostgresConnection, tableName string, postgresTableSchema *schemasv1alpha4.SQLTableSchema) ([]string, error) {
	query := `select
column_name, column_default, is_nullable, data_type, udt_name, character_maximum_length
from information_schema.columns
where table_name = $1`
	rows, err := p.db.Query(query, tableName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to select from information_schema")
	}

	alterAndDropStatements := []string{}
	foundColumnNames := []string{}
	for rows.Next() {
		var columnName, dataType, udtName, isNullable string
		var columnDefault sql.NullString
		var charMaxLength sql.NullInt64

		if err := rows.Scan(&columnName, &columnDefault, &isNullable, &dataType, &udtName, &charMaxLength); err != nil {
			return nil, errors.Wrap(err, "failed to scan")
		}

		foundColumnNames = append(foundColumnNames, columnName)

		existingColumn := types.Column{
			Name:        columnName,
			DataType:    dataType,
			Constraints: &types.ColumnConstraints{},
		}

		if dataType == "ARRAY" {
			existingColumn.IsArray = true
			existingColumn.DataType = UDTNameToDataType(udtName)
		}

		if isNullable == "NO" {
			existingColumn.Constraints.NotNull = &trueValue
		} else {
			existingColumn.Constraints.NotNull = &falseValue
		}

		if columnDefault.Valid {
			value := stripOIDClass(columnDefault.String)
			existingColumn.ColumnDefault = &value
		}
		if charMaxLength.Valid {
			existingColumn.DataType = fmt.Sprintf("%s (%d)", existingColumn.DataType, charMaxLength.Int64)
		}

		columnStatement, err := AlterColumnStatements(tableName, postgresTableSchema.PrimaryKey, postgresTableSchema.Columns, &existingColumn)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create alter column statement")
		}

		alterAndDropStatements = append(alterAndDropStatements, columnStatement...)
	}

	for _, desiredColumn := range postgresTableSchema.Columns {
		isColumnPresent := false
		for _, foundColumn := range foundColumnNames {
			if foundColumn == desiredColumn.Name {
				isColumnPresent = true
			}
		}

		if !isColumnPresent {
			statement, err := InsertColumnStatement(tableName, desiredColumn)
			if err != nil {
				return nil, errors.Wrap(err, "failed to create insert column statement")
			}

			alterAndDropStatements = append(alterAndDropStatements, statement)
		}
	}

	return alterAndDropStatements, nil
}

func buildPrimaryKeyStatements(p *PostgresConnection, tableName string, postgresTableSchema *schemasv1alpha4.SQLTableSchema) ([]string, error) {
	currentPrimaryKey, err := p.GetTablePrimaryKey(tableName)
	if err != nil {
		return nil, err
	}
	var postgresTableSchemaPrimaryKey *types.KeyConstraint
	if len(postgresTableSchema.PrimaryKey) > 0 {
		postgresTableSchemaPrimaryKey = &types.KeyConstraint{
			IsPrimary: true,
			Columns:   postgresTableSchema.PrimaryKey,
		}
	}

	if postgresTableSchemaPrimaryKey.Equals(currentPrimaryKey) {
		return nil, nil
	}

	var statements []string
	if currentPrimaryKey != nil {
		statements = append(statements, RemoveConstrantStatement(tableName, currentPrimaryKey))
	}

	if postgresTableSchemaPrimaryKey != nil {
		statements = append(statements, AddConstrantStatement(tableName, postgresTableSchemaPrimaryKey))
	}

	return statements, nil
}

func buildForeignKeyStatements(p *PostgresConnection, tableName string, postgresTableSchema *schemasv1alpha4.SQLTableSchema) ([]string, error) {
	foreignKeyStatements := []string{}
	droppedKeys := []string{}
	currentForeignKeys, err := p.ListTableForeignKeys(p.databaseName, tableName)
	if err != nil {
		return nil, err
	}

	for _, foreignKey := range postgresTableSchema.ForeignKeys {
		var statement string
		var matchedForeignKey *types.ForeignKey
		for _, currentForeignKey := range currentForeignKeys {
			if currentForeignKey.Equals(types.SchemaForeignKeyToForeignKey(foreignKey)) {
				goto Next
			}

			matchedForeignKey = currentForeignKey
		}

		// drop and readd?  is this always ok
		// TODO can we alter
		if matchedForeignKey != nil {
			statement = RemoveForeignKeyStatement(tableName, matchedForeignKey)
			droppedKeys = append(droppedKeys, matchedForeignKey.Name)
			foreignKeyStatements = append(foreignKeyStatements, statement)
		}

		statement = AddForeignKeyStatement(tableName, foreignKey)
		foreignKeyStatements = append(foreignKeyStatements, statement)

	Next:
	}

	for _, currentForeignKey := range currentForeignKeys {
		var statement string
		for _, foreignKey := range postgresTableSchema.ForeignKeys {
			if currentForeignKey.Equals(types.SchemaForeignKeyToForeignKey(foreignKey)) {
				goto NextCurrentFK
			}
		}

		for _, droppedKey := range droppedKeys {
			if droppedKey == currentForeignKey.Name {
				goto NextCurrentFK
			}
		}

		statement = RemoveForeignKeyStatement(tableName, currentForeignKey)
		foreignKeyStatements = append(foreignKeyStatements, statement)

	NextCurrentFK:
	}

	return foreignKeyStatements, nil
}

func buildIndexStatements(p *PostgresConnection, tableName string, postgresTableSchema *schemasv1alpha4.SQLTableSchema) ([]string, error) {
	indexStatements := []string{}
	droppedIndexes := []string{}
	currentIndexes, err := p.ListTableIndexes(p.databaseName, tableName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list table indexes")
	}
	currentConstraints, err := p.ListTableConstraints(p.databaseName, tableName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list table constraints")
	}

	for _, index := range postgresTableSchema.Indexes {
		if index.Name == "" {
			index.Name = types.GenerateIndexName(tableName, index)
		}

		var statement string
		var matchedIndex *types.Index
		for _, currentIndex := range currentIndexes {
			if currentIndex.Equals(types.SchemaIndexToIndex(index)) {
				goto Next
			}

			if currentIndex.Name == index.Name {
				matchedIndex = currentIndex
			}
		}

		// drop and readd? pg supports a little bit of alter index we should support (rename)
		if matchedIndex != nil {
			isConstraint := false
			for _, currentConstraint := range currentConstraints {
				if matchedIndex.Name == currentConstraint {
					isConstraint = true
				}
			}

			if isConstraint {
				statement = RemoveConstraintStatement(tableName, matchedIndex)
			} else {
				statement = RemoveIndexStatement(tableName, matchedIndex)
			}
			droppedIndexes = append(droppedIndexes, matchedIndex.Name)
			indexStatements = append(indexStatements, statement)
		}

		statement = AddIndexStatement(tableName, index)
		indexStatements = append(indexStatements, statement)

	Next:
	}

	for _, currentIndex := range currentIndexes {
		var statement string
		isConstraint := false

		for _, index := range postgresTableSchema.Indexes {
			if currentIndex.Equals(types.SchemaIndexToIndex(index)) {
				goto NextCurrentIdx
			}
		}

		for _, droppedIndex := range droppedIndexes {
			if droppedIndex == currentIndex.Name {
				goto NextCurrentIdx
			}
		}

		for _, currentConstraint := range currentConstraints {
			if currentIndex.Name == currentConstraint {
				isConstraint = true
			}
		}

		if isConstraint {
			statement = RemoveConstraintStatement(tableName, currentIndex)
		} else {
			statement = RemoveIndexStatement(tableName, currentIndex)
		}

		indexStatements = append(indexStatements, statement)

	NextCurrentIdx:
	}

	return indexStatements, nil
}
