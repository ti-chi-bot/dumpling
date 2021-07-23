// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	tcontext "github.com/pingcap/dumpling/v4/context"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb/store/helper"
	"go.uber.org/zap"
)

const orderByTiDBRowID = "ORDER BY `_tidb_rowid`"

// ShowDatabases shows the databases of a database server.
func ShowDatabases(db *sql.Conn) ([]string, error) {
	var res oneStrColumnTable
	if err := simpleQuery(db, "SHOW DATABASES", res.handleOneRow); err != nil {
		return nil, err
	}
	return res.data, nil
}

// ShowTables shows the tables of a database, the caller should use the correct database.
func ShowTables(db *sql.Conn) ([]string, error) {
	var res oneStrColumnTable
	if err := simpleQuery(db, "SHOW TABLES", res.handleOneRow); err != nil {
		return nil, err
	}
	return res.data, nil
}

// ShowCreateDatabase constructs the create database SQL for a specified database
// returns (createDatabaseSQL, error)
func ShowCreateDatabase(db *sql.Conn, database string) (string, error) {
	var oneRow [2]string
	handleOneRow := func(rows *sql.Rows) error {
		return rows.Scan(&oneRow[0], &oneRow[1])
	}
	query := fmt.Sprintf("SHOW CREATE DATABASE `%s`", escapeString(database))
	err := simpleQuery(db, query, handleOneRow)
	if err != nil {
		return "", errors.Annotatef(err, "sql: %s", query)
	}
	return oneRow[1], nil
}

// ShowCreateTable constructs the create table SQL for a specified table
// returns (createTableSQL, error)
func ShowCreateTable(db *sql.Conn, database, table string) (string, error) {
	var oneRow [2]string
	handleOneRow := func(rows *sql.Rows) error {
		return rows.Scan(&oneRow[0], &oneRow[1])
	}
	query := fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", escapeString(database), escapeString(table))
	err := simpleQuery(db, query, handleOneRow)
	if err != nil {
		return "", errors.Annotatef(err, "sql: %s", query)
	}
	return oneRow[1], nil
}

// ShowCreateView constructs the create view SQL for a specified view
// returns (createFakeTableSQL, createViewSQL, error)
func ShowCreateView(db *sql.Conn, database, view string) (createFakeTableSQL string, createRealViewSQL string, err error) {
	var fieldNames []string
	handleFieldRow := func(rows *sql.Rows) error {
		var oneRow [6]sql.NullString
		scanErr := rows.Scan(&oneRow[0], &oneRow[1], &oneRow[2], &oneRow[3], &oneRow[4], &oneRow[5])
		if scanErr != nil {
			return errors.Trace(scanErr)
		}
		if oneRow[0].Valid {
			fieldNames = append(fieldNames, fmt.Sprintf("`%s` int", escapeString(oneRow[0].String)))
		}
		return nil
	}
	var oneRow [4]string
	handleOneRow := func(rows *sql.Rows) error {
		return rows.Scan(&oneRow[0], &oneRow[1], &oneRow[2], &oneRow[3])
	}
	var createTableSQL, createViewSQL strings.Builder

	// Build createTableSQL
	query := fmt.Sprintf("SHOW FIELDS FROM `%s`.`%s`", escapeString(database), escapeString(view))
	err = simpleQuery(db, query, handleFieldRow)
	if err != nil {
		return "", "", errors.Annotatef(err, "sql: %s", query)
	}
	fmt.Fprintf(&createTableSQL, "CREATE TABLE `%s`(\n", escapeString(view))
	createTableSQL.WriteString(strings.Join(fieldNames, ",\n"))
	createTableSQL.WriteString("\n)ENGINE=MyISAM;\n")

	// Build createViewSQL
	fmt.Fprintf(&createViewSQL, "DROP TABLE IF EXISTS `%s`;\n", escapeString(view))
	fmt.Fprintf(&createViewSQL, "DROP VIEW IF EXISTS `%s`;\n", escapeString(view))
	query = fmt.Sprintf("SHOW CREATE VIEW `%s`.`%s`", escapeString(database), escapeString(view))
	err = simpleQuery(db, query, handleOneRow)
	if err != nil {
		return "", "", errors.Annotatef(err, "sql: %s", query)
	}
	// The result for `show create view` SQL
	// mysql> show create view v1;
	// +------+-------------------------------------------------------------------------------------------------------------------------------------+----------------------+----------------------+
	// | View | Create View                                                                                                                         | character_set_client | collation_connection |
	// +------+-------------------------------------------------------------------------------------------------------------------------------------+----------------------+----------------------+
	// | v1   | CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v1` (`a`) AS SELECT `t`.`a` AS `a` FROM `test`.`t` | utf8                 | utf8_general_ci      |
	// +------+-------------------------------------------------------------------------------------------------------------------------------------+----------------------+----------------------+
	SetCharset(&createViewSQL, oneRow[2], oneRow[3])
	createViewSQL.WriteString(oneRow[1])
	createViewSQL.WriteString(";\n")
	RestoreCharset(&createViewSQL)

	return createTableSQL.String(), createViewSQL.String(), nil
}

// SetCharset builds the set charset SQLs
func SetCharset(w *strings.Builder, characterSet, collationConnection string) {
	w.WriteString("SET @PREV_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT;\n")
	w.WriteString("SET @PREV_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS;\n")
	w.WriteString("SET @PREV_COLLATION_CONNECTION=@@COLLATION_CONNECTION;\n")

	fmt.Fprintf(w, "SET character_set_client = %s;\n", characterSet)
	fmt.Fprintf(w, "SET character_set_results = %s;\n", characterSet)
	fmt.Fprintf(w, "SET collation_connection = %s;\n", collationConnection)
}

// RestoreCharset builds the restore charset SQLs
func RestoreCharset(w io.StringWriter) {
	_, _ = w.WriteString("SET character_set_client = @PREV_CHARACTER_SET_CLIENT;\n")
	_, _ = w.WriteString("SET character_set_results = @PREV_CHARACTER_SET_RESULTS;\n")
	_, _ = w.WriteString("SET collation_connection = @PREV_COLLATION_CONNECTION;\n")
}

// ListAllDatabasesTables lists all the databases and tables from the database
// if asap is true, will use information_schema to get table info in one query
// if asap is false, will use show table status for each database because it has better performance according to our tests
func ListAllDatabasesTables(tctx *tcontext.Context, db *sql.Conn, databaseNames []string, asap bool, tableTypes ...TableType) (DatabaseTables, error) { // revive:disable-line:flag-parameter
	dbTables := DatabaseTables{}
	var (
		schema, table, tableTypeStr string
		tableType                   TableType
		avgRowLength                uint64
		err                         error
	)
	if asap {
		tableTypeConditions := make([]string, len(tableTypes))
		for i, tableType := range tableTypes {
			tableTypeConditions[i] = fmt.Sprintf("TABLE_TYPE='%s'", tableType)
		}
		query := fmt.Sprintf("SELECT TABLE_SCHEMA,TABLE_NAME,TABLE_TYPE,AVG_ROW_LENGTH FROM INFORMATION_SCHEMA.TABLES WHERE %s", strings.Join(tableTypeConditions, " OR "))
		for _, schema := range databaseNames {
			dbTables[schema] = make([]*TableInfo, 0)
		}
		if err = simpleQueryWithArgs(db, func(rows *sql.Rows) error {
			var (
				sqlAvgRowLength sql.NullInt64
				err2            error
			)
			if err2 = rows.Scan(&schema, &table, &tableTypeStr, &sqlAvgRowLength); err != nil {
				return errors.Trace(err2)
			}
			tableType, err2 = ParseTableType(tableTypeStr)
			if err2 != nil {
				return errors.Trace(err2)
			}

			if sqlAvgRowLength.Valid {
				avgRowLength = uint64(sqlAvgRowLength.Int64)
			} else {
				avgRowLength = 0
			}
			// only append tables to schemas in databaseNames
			if _, ok := dbTables[schema]; ok {
				dbTables[schema] = append(dbTables[schema], &TableInfo{table, avgRowLength, tableType})
			}
			return nil
		}, query); err != nil {
			return nil, errors.Annotatef(err, "sql: %s", query)
		}
	} else {
		queryTemplate := "SHOW TABLE STATUS FROM `%s`"
		selectedTableType := make(map[TableType]struct{})
		for _, tableType = range tableTypes {
			selectedTableType[tableType] = struct{}{}
		}
		for _, schema = range databaseNames {
			dbTables[schema] = make([]*TableInfo, 0)
			query := fmt.Sprintf(queryTemplate, escapeString(schema))
			rows, err := db.QueryContext(tctx, query)
			if err != nil {
				return nil, errors.Annotatef(err, "sql: %s", query)
			}
			results, err := GetSpecifiedColumnValuesAndClose(rows, "NAME", "ENGINE", "AVG_ROW_LENGTH", "COMMENT")
			if err != nil {
				return nil, errors.Annotatef(err, "sql: %s", query)
			}
			for _, oneRow := range results {
				table, engine, avgRowLengthStr, comment := oneRow[0], oneRow[1], oneRow[2], oneRow[3]
				if avgRowLengthStr != "" {
					avgRowLength, err = strconv.ParseUint(avgRowLengthStr, 10, 64)
					if err != nil {
						return nil, errors.Annotatef(err, "sql: %s", query)
					}
				} else {
					avgRowLength = 0
				}
				tableType = TableTypeBase
				if engine == "" && (comment == "" || comment == TableTypeViewStr) {
					tableType = TableTypeView
				} else if engine == "" {
					tctx.L().Warn("Invalid table without engine found", zap.String("database", schema), zap.String("table", table))
					continue
				}
				if _, ok := selectedTableType[tableType]; !ok {
					continue
				}
				dbTables[schema] = append(dbTables[schema], &TableInfo{table, avgRowLength, tableType})
			}
		}
	}
	return dbTables, nil
}

// SelectVersion gets the version information from the database server
func SelectVersion(db *sql.DB) (string, error) {
	var versionInfo string
	const query = "SELECT version()"
	row := db.QueryRow(query)
	err := row.Scan(&versionInfo)
	if err != nil {
		return "", errors.Annotatef(err, "sql: %s", query)
	}
	return versionInfo, nil
}

// SelectAllFromTable dumps data serialized from a specified table
func SelectAllFromTable(conf *Config, meta TableMeta, partition, orderByClause string) TableDataIR {
	database, table := meta.DatabaseName(), meta.TableName()
	selectedField, selectLen := meta.SelectedField(), meta.SelectedLen()
	query := buildSelectQuery(database, table, selectedField, partition, buildWhereCondition(conf, ""), orderByClause)

	return &tableData{
		query:  query,
		colLen: selectLen,
	}
}

func buildSelectQuery(database, table, fields, partition, where, orderByClause string) string {
	var query strings.Builder
	query.WriteString("SELECT ")
	if fields == "" {
		// If all of the columns are generated,
		// we need to make sure the query is valid.
		fields = "''"
	}
	query.WriteString(fields)
	query.WriteString(" FROM `")
	query.WriteString(escapeString(database))
	query.WriteString("`.`")
	query.WriteString(escapeString(table))
	query.WriteByte('`')
	if partition != "" {
		query.WriteString(" PARTITION(`")
		query.WriteString(escapeString(partition))
		query.WriteString("`)")
	}

	if where != "" {
		query.WriteString(" ")
		query.WriteString(where)
	}

	if orderByClause != "" {
		query.WriteString(" ")
		query.WriteString(orderByClause)
	}

	return query.String()
}

func buildOrderByClause(conf *Config, db *sql.Conn, database, table string, hasImplicitRowID bool) (string, error) { // revive:disable-line:flag-parameter
	if !conf.SortByPk {
		return "", nil
	}
	if hasImplicitRowID {
		return orderByTiDBRowID, nil
	}
	cols, err := GetPrimaryKeyColumns(db, database, table)
	if err != nil {
		return "", errors.Trace(err)
	}
	return buildOrderByClauseString(cols), nil
}

// SelectTiDBRowID checks whether this table has _tidb_rowid column
func SelectTiDBRowID(db *sql.Conn, database, table string) (bool, error) {
	const errBadFieldCode = 1054
	tiDBRowIDQuery := fmt.Sprintf("SELECT _tidb_rowid from `%s`.`%s` LIMIT 0", escapeString(database), escapeString(table))
	_, err := db.ExecContext(context.Background(), tiDBRowIDQuery)
	if err != nil {
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, fmt.Sprintf("%d", errBadFieldCode)) {
			return false, nil
		}
		return false, errors.Annotatef(err, "sql: %s", tiDBRowIDQuery)
	}
	return true, nil
}

// GetSuitableRows gets suitable rows for each table
func GetSuitableRows(avgRowLength uint64) uint64 {
	const (
		defaultRows  = 200000
		maxRows      = 1000000
		bytesPerFile = 128 * 1024 * 1024 // 128MB per file by default
	)
	if avgRowLength == 0 {
		return defaultRows
	}
	estimateRows := bytesPerFile / avgRowLength
	if estimateRows > maxRows {
		return maxRows
	}
	return estimateRows
}

// GetColumnTypes gets *sql.ColumnTypes from a specified table
func GetColumnTypes(db *sql.Conn, fields, database, table string) ([]*sql.ColumnType, error) {
	query := fmt.Sprintf("SELECT %s FROM `%s`.`%s` LIMIT 1", fields, escapeString(database), escapeString(table))
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", query)
	}
	defer rows.Close()
	if err = rows.Err(); err != nil {
		return nil, errors.Annotatef(err, "sql: %s", query)
	}
	return rows.ColumnTypes()
}

// GetPrimaryKeyAndColumnTypes gets all primary columns and their types in ordinal order
func GetPrimaryKeyAndColumnTypes(conn *sql.Conn, meta TableMeta) ([]string, []string, error) {
	var (
		colNames, colTypes []string
		err                error
	)
	colNames, err = GetPrimaryKeyColumns(conn, meta.DatabaseName(), meta.TableName())
	if err != nil {
		return nil, nil, err
	}
	colName2Type := string2Map(meta.ColumnNames(), meta.ColumnTypes())
	colTypes = make([]string, len(colNames))
	for i, colName := range colNames {
		colTypes[i] = colName2Type[colName]
	}
	return colNames, colTypes, nil
}

// GetPrimaryKeyColumns gets all primary columns in ordinal order
func GetPrimaryKeyColumns(db *sql.Conn, database, table string) ([]string, error) {
	priKeyColsQuery := fmt.Sprintf("SHOW INDEX FROM `%s`.`%s`", escapeString(database), escapeString(table))
	rows, err := db.QueryContext(context.Background(), priKeyColsQuery)
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", priKeyColsQuery)
	}
	results, err := GetSpecifiedColumnValuesAndClose(rows, "KEY_NAME", "COLUMN_NAME")
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", priKeyColsQuery)
	}
	cols := make([]string, 0, len(results))
	for _, oneRow := range results {
		keyName, columnName := oneRow[0], oneRow[1]
		if keyName == "PRIMARY" {
			cols = append(cols, columnName)
		}
	}
	return cols, nil
}

func getNumericIndex(db *sql.Conn, meta TableMeta) (string, error) {
	database, table := meta.DatabaseName(), meta.TableName()
	colName2Type := string2Map(meta.ColumnNames(), meta.ColumnTypes())
	keyQuery := fmt.Sprintf("SHOW INDEX FROM `%s`.`%s`", escapeString(database), escapeString(table))
	rows, err := db.QueryContext(context.Background(), keyQuery)
	if err != nil {
		return "", errors.Annotatef(err, "sql: %s", keyQuery)
	}
	results, err := GetSpecifiedColumnValuesAndClose(rows, "NON_UNIQUE", "KEY_NAME", "COLUMN_NAME")
	if err != nil {
		return "", errors.Annotatef(err, "sql: %s", keyQuery)
	}
	uniqueColumnName := ""
	// check primary key first, then unique key
	for _, oneRow := range results {
		var ok bool
		if _, ok = dataTypeNum[colName2Type[oneRow[2]]]; ok && oneRow[1] == "PRIMARY" {
			return oneRow[2], nil
		}
		if uniqueColumnName != "" && oneRow[0] == "0" && ok {
			uniqueColumnName = oneRow[2]
		}
	}
	return uniqueColumnName, nil
}

// FlushTableWithReadLock flush tables with read lock
func FlushTableWithReadLock(ctx context.Context, db *sql.Conn) error {
	const ftwrlQuery = "FLUSH TABLES WITH READ LOCK"
	_, err := db.ExecContext(ctx, ftwrlQuery)
	return errors.Annotatef(err, "sql: %s", ftwrlQuery)
}

// LockTables locks table with read lock
func LockTables(ctx context.Context, db *sql.Conn, database, table string) error {
	lockTableQuery := fmt.Sprintf("LOCK TABLES `%s`.`%s` READ", escapeString(database), escapeString(table))
	_, err := db.ExecContext(ctx, lockTableQuery)
	return errors.Annotatef(err, "sql: %s", lockTableQuery)
}

// UnlockTables unlocks all tables' lock
func UnlockTables(ctx context.Context, db *sql.Conn) error {
	const unlockTableQuery = "UNLOCK TABLES"
	_, err := db.ExecContext(ctx, unlockTableQuery)
	return errors.Annotatef(err, "sql: %s", unlockTableQuery)
}

// ShowMasterStatus get SHOW MASTER STATUS result from database
func ShowMasterStatus(db *sql.Conn) ([]string, error) {
	var oneRow []string
	handleOneRow := func(rows *sql.Rows) error {
		cols, err := rows.Columns()
		if err != nil {
			return errors.Trace(err)
		}
		fieldNum := len(cols)
		oneRow = make([]string, fieldNum)
		addr := make([]interface{}, fieldNum)
		for i := range oneRow {
			addr[i] = &oneRow[i]
		}
		return rows.Scan(addr...)
	}
	const showMasterStatusQuery = "SHOW MASTER STATUS"
	err := simpleQuery(db, showMasterStatusQuery, handleOneRow)
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", showMasterStatusQuery)
	}
	return oneRow, nil
}

// GetSpecifiedColumnValueAndClose get columns' values whose name is equal to columnName and close the given rows
func GetSpecifiedColumnValueAndClose(rows *sql.Rows, columnName string) ([]string, error) {
	if rows == nil {
		return []string{}, nil
	}
	defer rows.Close()
	columnName = strings.ToUpper(columnName)
	var strs []string
	columns, _ := rows.Columns()
	addr := make([]interface{}, len(columns))
	oneRow := make([]sql.NullString, len(columns))
	fieldIndex := -1
	for i, col := range columns {
		if strings.ToUpper(col) == columnName {
			fieldIndex = i
		}
		addr[i] = &oneRow[i]
	}
	if fieldIndex == -1 {
		return strs, nil
	}
	for rows.Next() {
		err := rows.Scan(addr...)
		if err != nil {
			return strs, errors.Trace(err)
		}
		if oneRow[fieldIndex].Valid {
			strs = append(strs, oneRow[fieldIndex].String)
		}
	}
	return strs, errors.Trace(rows.Err())
}

// GetSpecifiedColumnValuesAndClose get columns' values whose name is equal to columnName
func GetSpecifiedColumnValuesAndClose(rows *sql.Rows, columnName ...string) ([][]string, error) {
	if rows == nil {
		return [][]string{}, nil
	}
	defer rows.Close()
	var strs [][]string
	columns, err := rows.Columns()
	if err != nil {
		return strs, errors.Trace(err)
	}
	addr := make([]interface{}, len(columns))
	oneRow := make([]sql.NullString, len(columns))
	fieldIndexMp := make(map[int]int)
	for i, col := range columns {
		addr[i] = &oneRow[i]
		for j, name := range columnName {
			if strings.ToUpper(col) == name {
				fieldIndexMp[i] = j
			}
		}
	}
	if len(fieldIndexMp) == 0 {
		return strs, nil
	}
	for rows.Next() {
		err := rows.Scan(addr...)
		if err != nil {
			return strs, errors.Trace(err)
		}
		written := false
		tmpStr := make([]string, len(columnName))
		for colPos, namePos := range fieldIndexMp {
			if oneRow[colPos].Valid {
				written = true
				tmpStr[namePos] = oneRow[colPos].String
			}
		}
		if written {
			strs = append(strs, tmpStr)
		}
	}
	return strs, errors.Trace(rows.Err())
}

// GetPdAddrs gets PD address from TiDB
func GetPdAddrs(tctx *tcontext.Context, db *sql.DB) ([]string, error) {
	query := "SELECT * FROM information_schema.cluster_info where type = 'pd';"
	rows, err := db.QueryContext(tctx, query)
	if err != nil {
		tctx.L().Warn("can't execute query from db",
			zap.String("query", query), zap.Error(err))
		return []string{}, errors.Annotatef(err, "sql: %s", query)
	}
	return GetSpecifiedColumnValueAndClose(rows, "STATUS_ADDRESS")
}

// GetTiDBDDLIDs gets DDL IDs from TiDB
func GetTiDBDDLIDs(tctx *tcontext.Context, db *sql.DB) ([]string, error) {
	query := "SELECT * FROM information_schema.tidb_servers_info;"
	rows, err := db.QueryContext(tctx, query)
	if err != nil {
		tctx.L().Warn("can't execute query from db",
			zap.String("query", query), zap.Error(err))
		return []string{}, errors.Annotatef(err, "sql: %s", query)
	}
	return GetSpecifiedColumnValueAndClose(rows, "DDL_ID")
}

// CheckTiDBWithTiKV use sql to check whether current TiDB has TiKV
func CheckTiDBWithTiKV(db *sql.DB) (bool, error) {
	var count int
	query := "SELECT COUNT(1) as c FROM MYSQL.TiDB WHERE VARIABLE_NAME='tikv_gc_safe_point'"
	row := db.QueryRow(query)
	err := row.Scan(&count)
	if err != nil {
		return false, errors.Annotatef(err, "sql: %s", query)
	}
	return count > 0, nil
}

func getSnapshot(db *sql.Conn) (string, error) {
	str, err := ShowMasterStatus(db)
	if err != nil {
		return "", err
	}
	return str[snapshotFieldIndex], nil
}

func isUnknownSystemVariableErr(err error) bool {
	return strings.Contains(err.Error(), "Unknown system variable")
}

func resetDBWithSessionParams(tctx *tcontext.Context, db *sql.DB, dsn string, params map[string]interface{}) (*sql.DB, error) {
	support := make(map[string]interface{})
	for k, v := range params {
		var pv interface{}
		if str, ok := v.(string); ok {
			if pvi, err := strconv.ParseInt(str, 10, 64); err == nil {
				pv = pvi
			} else if pvf, err := strconv.ParseFloat(str, 64); err == nil {
				pv = pvf
			} else {
				pv = str
			}
		} else {
			pv = v
		}
		s := fmt.Sprintf("SET SESSION %s = ?", k)
		_, err := db.ExecContext(tctx, s, pv)
		if err != nil {
			if isUnknownSystemVariableErr(err) {
				tctx.L().Info("session variable is not supported by db", zap.String("variable", k), zap.Reflect("value", v))
				continue
			}
			return nil, errors.Trace(err)
		}

		support[k] = pv
	}

	for k, v := range support {
		var s string
		// Wrap string with quote to handle string with space. For example, '2020-10-20 13:41:40'
		// For --params argument, quote doesn't matter because it doesn't affect the actual value
		if str, ok := v.(string); ok {
			s = wrapStringWith(str, "'")
		} else {
			s = fmt.Sprintf("%v", v)
		}
		dsn += fmt.Sprintf("&%s=%s", k, url.QueryEscape(s))
	}

	newDB, err := sql.Open("mysql", dsn)
	db.Close()
	return newDB, errors.Trace(err)
}

func createConnWithConsistency(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	query := "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"
	_, err = conn.ExecContext(ctx, query)
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", query)
	}
	query = "START TRANSACTION /*!40108 WITH CONSISTENT SNAPSHOT */"
	_, err = conn.ExecContext(ctx, query)
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", query)
	}
	return conn, nil
}

// buildSelectField returns the selecting fields' string(joined by comma(`,`)),
// and the number of writable fields.
func buildSelectField(db *sql.Conn, dbName, tableName string, completeInsert bool) (string, int, error) { // revive:disable-line:flag-parameter
	query := fmt.Sprintf("SHOW COLUMNS FROM `%s`.`%s`", escapeString(dbName), escapeString(tableName))
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		return "", 0, errors.Annotatef(err, "sql: %s", query)
	}
	defer rows.Close()
	availableFields := make([]string, 0)

	hasGenerateColumn := false
	results, err := GetSpecifiedColumnValuesAndClose(rows, "FIELD", "EXTRA")
	if err != nil {
		return "", 0, errors.Annotatef(err, "sql: %s", query)
	}
	for _, oneRow := range results {
		fieldName, extra := oneRow[0], oneRow[1]
		switch extra {
		case "STORED GENERATED", "VIRTUAL GENERATED":
			hasGenerateColumn = true
			continue
		}
		availableFields = append(availableFields, wrapBackTicks(escapeString(fieldName)))
	}
	if completeInsert || hasGenerateColumn {
		return strings.Join(availableFields, ","), len(availableFields), nil
	}
	return "*", len(availableFields), nil
}

func buildWhereClauses(handleColNames []string, handleVals [][]string) []string {
	if len(handleColNames) == 0 || len(handleVals) == 0 {
		return nil
	}
	quotaCols := make([]string, len(handleColNames))
	for i, s := range handleColNames {
		quotaCols[i] = fmt.Sprintf("`%s`", escapeString(s))
	}
	where := make([]string, 0, len(handleVals)+1)
	buf := &bytes.Buffer{}
	buildCompareClause(buf, quotaCols, handleVals[0], less, false)
	where = append(where, buf.String())
	buf.Reset()
	for i := 1; i < len(handleVals); i++ {
		low, up := handleVals[i-1], handleVals[i]
		buildBetweenClause(buf, quotaCols, low, up)
		where = append(where, buf.String())
		buf.Reset()
	}
	buildCompareClause(buf, quotaCols, handleVals[len(handleVals)-1], greater, true)
	where = append(where, buf.String())
	buf.Reset()
	return where
}

// return greater than TableRangeScan where clause
// the result doesn't contain brackets
const (
	greater = '>'
	less    = '<'
	equal   = '='
)

// buildCompareClause build clause with specified bounds. Usually we will use the following two conditions:
// (compare, writeEqual) == (less, false), return quotaCols < bound clause. In other words, (-inf, bound)
// (compare, writeEqual) == (greater, true), return quotaCols >= bound clause. In other words, [bound, +inf)
func buildCompareClause(buf *bytes.Buffer, quotaCols []string, bound []string, compare byte, writeEqual bool) { // revive:disable-line:flag-parameter
	for i, col := range quotaCols {
		if i > 0 {
			buf.WriteString("or(")
		}
		for j := 0; j < i; j++ {
			buf.WriteString(quotaCols[j])
			buf.WriteByte(equal)
			buf.WriteString(bound[j])
			buf.WriteString(" and ")
		}
		buf.WriteString(col)
		buf.WriteByte(compare)
		if writeEqual && i == len(quotaCols)-1 {
			buf.WriteByte(equal)
		}
		buf.WriteString(bound[i])
		if i > 0 {
			buf.WriteByte(')')
		} else if i != len(quotaCols)-1 {
			buf.WriteByte(' ')
		}
	}
}

// getCommonLength returns the common length of low and up
func getCommonLength(low []string, up []string) int {
	for i := range low {
		if low[i] != up[i] {
			return i
		}
	}
	return len(low)
}

// buildBetweenClause build clause in a specified table range.
// the result where clause will be low <= quotaCols < up. In other words, [low, up)
func buildBetweenClause(buf *bytes.Buffer, quotaCols []string, low []string, up []string) {
	singleBetween := func(writeEqual bool) {
		buf.WriteString(quotaCols[0])
		buf.WriteByte(greater)
		if writeEqual {
			buf.WriteByte(equal)
		}
		buf.WriteString(low[0])
		buf.WriteString(" and ")
		buf.WriteString(quotaCols[0])
		buf.WriteByte(less)
		buf.WriteString(up[0])
	}
	// handle special cases with common prefix
	commonLen := getCommonLength(low, up)
	if commonLen > 0 {
		// unexpected case for low == up, return empty result
		if commonLen == len(low) {
			buf.WriteString("false")
			return
		}
		for i := 0; i < commonLen; i++ {
			if i > 0 {
				buf.WriteString(" and ")
			}
			buf.WriteString(quotaCols[i])
			buf.WriteByte(equal)
			buf.WriteString(low[i])
		}
		buf.WriteString(" and(")
		defer buf.WriteByte(')')
		quotaCols = quotaCols[commonLen:]
		low = low[commonLen:]
		up = up[commonLen:]
	}

	// handle special cases with only one column
	if len(quotaCols) == 1 {
		singleBetween(true)
		return
	}
	buf.WriteByte('(')
	singleBetween(false)
	buf.WriteString(")or(")
	buf.WriteString(quotaCols[0])
	buf.WriteByte(equal)
	buf.WriteString(low[0])
	buf.WriteString(" and(")
	buildCompareClause(buf, quotaCols[1:], low[1:], greater, true)
	buf.WriteString("))or(")
	buf.WriteString(quotaCols[0])
	buf.WriteByte(equal)
	buf.WriteString(up[0])
	buf.WriteString(" and(")
	buildCompareClause(buf, quotaCols[1:], up[1:], less, false)
	buf.WriteString("))")
}

func buildOrderByClauseString(handleColNames []string) string {
	if len(handleColNames) == 0 {
		return ""
	}
	separator := ","
	quotaCols := make([]string, len(handleColNames))
	for i, col := range handleColNames {
		quotaCols[i] = fmt.Sprintf("`%s`", escapeString(col))
	}
	return fmt.Sprintf("ORDER BY %s", strings.Join(quotaCols, separator))
}

func buildLockTablesSQL(allTables DatabaseTables, blockList map[string]map[string]interface{}) string {
	// ,``.`` READ has 11 bytes, "LOCK TABLE" has 10 bytes
	estimatedCap := len(allTables)*11 + 10
	s := bytes.NewBuffer(make([]byte, 0, estimatedCap))
	n := false
	for dbName, tables := range allTables {
		escapedDBName := escapeString(dbName)
		for _, table := range tables {
			// Lock views will lock related tables. However, we won't dump data only the create sql of view, so we needn't lock view here.
			// Besides, mydumper also only lock base table here. https://github.com/maxbube/mydumper/blob/1fabdf87e3007e5934227b504ad673ba3697946c/mydumper.c#L1568
			if table.Type != TableTypeBase {
				continue
			}
			if blockTable, ok := blockList[dbName]; ok {
				if _, ok := blockTable[table.Name]; ok {
					continue
				}
			}
			if !n {
				fmt.Fprintf(s, "LOCK TABLES `%s`.`%s` READ", escapedDBName, escapeString(table.Name))
				n = true
			} else {
				fmt.Fprintf(s, ",`%s`.`%s` READ", escapedDBName, escapeString(table.Name))
			}
		}
	}
	return s.String()
}

type oneStrColumnTable struct {
	data []string
}

func (o *oneStrColumnTable) handleOneRow(rows *sql.Rows) error {
	var str string
	if err := rows.Scan(&str); err != nil {
		return errors.Trace(err)
	}
	o.data = append(o.data, str)
	return nil
}

func simpleQuery(conn *sql.Conn, sql string, handleOneRow func(*sql.Rows) error) error {
	return simpleQueryWithArgs(conn, handleOneRow, sql)
}

func simpleQueryWithArgs(conn *sql.Conn, handleOneRow func(*sql.Rows) error, sql string, args ...interface{}) error {
	rows, err := conn.QueryContext(context.Background(), sql, args...)
	if err != nil {
		return errors.Annotatef(err, "sql: %s, args: %s", sql, args)
	}
	defer rows.Close()

	for rows.Next() {
		if err := handleOneRow(rows); err != nil {
			rows.Close()
			return errors.Annotatef(err, "sql: %s, args: %s", sql, args)
		}
	}
	rows.Close()
	return errors.Annotatef(rows.Err(), "sql: %s, args: %s", sql, args)
}

func pickupPossibleField(meta TableMeta, db *sql.Conn) (string, error) {
	// try using _tidb_rowid first
	if meta.HasImplicitRowID() {
		return "_tidb_rowid", nil
	}
	// try to use pk
	fieldName, err := getNumericIndex(db, meta)
	if err != nil {
		return "", err
	}

	// if fieldName == "", there is no proper index
	return fieldName, nil
}

func estimateCount(tctx *tcontext.Context, dbName, tableName string, db *sql.Conn, field string, conf *Config) uint64 {
	var query string
	if strings.TrimSpace(field) == "*" || strings.TrimSpace(field) == "" {
		query = fmt.Sprintf("EXPLAIN SELECT * FROM `%s`.`%s`", escapeString(dbName), escapeString(tableName))
	} else {
		query = fmt.Sprintf("EXPLAIN SELECT `%s` FROM `%s`.`%s`", escapeString(field), escapeString(dbName), escapeString(tableName))
	}

	if conf.Where != "" {
		query += " WHERE "
		query += conf.Where
	}

	estRows := detectEstimateRows(tctx, db, query, []string{"rows", "estRows", "count"})
	/* tidb results field name is estRows (before 4.0.0-beta.2: count)
		+-----------------------+----------+-----------+---------------------------------------------------------+
		| id                    | estRows  | task      | access object | operator info                           |
		+-----------------------+----------+-----------+---------------------------------------------------------+
		| tablereader_5         | 10000.00 | root      |               | data:tablefullscan_4                    |
		| └─tablefullscan_4     | 10000.00 | cop[tikv] | table:a       | table:a, keep order:false, stats:pseudo |
		+-----------------------+----------+-----------+----------------------------------------------------------

	mariadb result field name is rows
		+------+-------------+---------+-------+---------------+------+---------+------+----------+-------------+
		| id   | select_type | table   | type  | possible_keys | key  | key_len | ref  | rows     | Extra       |
		+------+-------------+---------+-------+---------------+------+---------+------+----------+-------------+
		|    1 | SIMPLE      | sbtest1 | index | NULL          | k_1  | 4       | NULL | 15000049 | Using index |
		+------+-------------+---------+-------+---------------+------+---------+------+----------+-------------+

	mysql result field name is rows
		+----+-------------+-------+------------+-------+---------------+-----------+---------+------+------+----------+-------------+
		| id | select_type | table | partitions | type  | possible_keys | key       | key_len | ref  | rows | filtered | Extra       |
		+----+-------------+-------+------------+-------+---------------+-----------+---------+------+------+----------+-------------+
		|  1 | SIMPLE      | t1    | NULL       | index | NULL          | multi_col | 10      | NULL |    5 |   100.00 | Using index |
		+----+-------------+-------+------------+-------+---------------+-----------+---------+------+------+----------+-------------+
	*/
	if estRows > 0 {
		return estRows
	}
	return 0
}

func detectEstimateRows(tctx *tcontext.Context, db *sql.Conn, query string, fieldNames []string) uint64 {
	rows, err := db.QueryContext(tctx, query)
	if err != nil {
		tctx.L().Warn("can't execute query from db",
			zap.String("query", query), zap.Error(err))
		return 0
	}
	defer rows.Close()
	rows.Next()
	columns, err := rows.Columns()
	if err != nil {
		tctx.L().Warn("can't get columns from db",
			zap.String("query", query), zap.Error(err))
		return 0
	}
	err = rows.Err()
	if err != nil {
		tctx.L().Warn("rows meet some error during the query",
			zap.String("query", query), zap.Error(err))
		return 0
	}
	addr := make([]interface{}, len(columns))
	oneRow := make([]sql.NullString, len(columns))
	fieldIndex := -1
	for i := range oneRow {
		addr[i] = &oneRow[i]
	}
found:
	for i := range oneRow {
		for _, fieldName := range fieldNames {
			if strings.EqualFold(columns[i], fieldName) {
				fieldIndex = i
				break found
			}
		}
	}
	err = rows.Scan(addr...)
	if err != nil || fieldIndex < 0 {
		tctx.L().Warn("can't get estimate count from db",
			zap.String("query", query), zap.Error(err))
		return 0
	}

	estRows, err := strconv.ParseFloat(oneRow[fieldIndex].String, 64)
	if err != nil {
		tctx.L().Warn("can't get parse rows from db",
			zap.String("query", query), zap.Error(err))
		return 0
	}
	return uint64(estRows)
}

func parseSnapshotToTSO(pool *sql.DB, snapshot string) (uint64, error) {
	snapshotTS, err := strconv.ParseUint(snapshot, 10, 64)
	if err == nil {
		return snapshotTS, nil
	}
	var tso sql.NullInt64
	query := "SELECT unix_timestamp(?)"
	row := pool.QueryRow(query, snapshot)
	err = row.Scan(&tso)
	if err != nil {
		return 0, errors.Annotatef(err, "sql: %s", strings.ReplaceAll(query, "?", fmt.Sprintf(`"%s"`, snapshot)))
	}
	if !tso.Valid {
		return 0, errors.Errorf("snapshot %s format not supported. please use tso or '2006-01-02 15:04:05' format time", snapshot)
	}
	return (uint64(tso.Int64) << 18) * 1000, nil
}

func buildWhereCondition(conf *Config, where string) string {
	var query strings.Builder
	separator := "WHERE"
	if conf.Where != "" {
		query.WriteString(separator)
		query.WriteByte(' ')
		query.WriteString(conf.Where)
		query.WriteByte(' ')
		separator = "AND"
		query.WriteByte(' ')
	}
	if where != "" {
		query.WriteString(separator)
		query.WriteString(" ")
		query.WriteString(where)
	}
	return query.String()
}

func escapeString(s string) string {
	return strings.ReplaceAll(s, "`", "``")
}

// GetPartitionNames get partition names from a specified table
func GetPartitionNames(db *sql.Conn, schema, table string) (partitions []string, err error) {
	partitions = make([]string, 0)
	var partitionName sql.NullString
	err = simpleQueryWithArgs(db, func(rows *sql.Rows) error {
		err := rows.Scan(&partitionName)
		if err != nil {
			return errors.Trace(err)
		}
		if partitionName.Valid {
			partitions = append(partitions, partitionName.String)
		}
		return nil
	}, "SELECT PARTITION_NAME from INFORMATION_SCHEMA.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", schema, table)
	return
}

// GetPartitionTableIDs get partition tableIDs through histograms.
// SHOW STATS_HISTOGRAMS  has db_name,table_name,partition_name but doesn't have partition id
// mysql.stats_histograms has partition_id but doesn't have db_name,table_name,partition_name
// So we combine the results from these two sqls to get partition ids for each table
// If UPDATE_TIME,DISTINCT_COUNT are equal, we assume these two records can represent one line.
// If histograms are not accurate or (UPDATE_TIME,DISTINCT_COUNT) has duplicate data, it's still fine.
// Because the possibility is low and the effect is that we will select more than one regions in one time,
// this will not affect the correctness of the dumping data and will not affect the memory usage much.
// This method is tricky, but no better way is found.
// Because TiDB v3.0.0's information_schema.partition table doesn't have partition name or partition id info
// return (dbName -> tbName -> partitionName -> partitionID, error)
func GetPartitionTableIDs(db *sql.Conn, tables map[string]map[string]struct{}) (map[string]map[string]map[string]int64, error) {
	const (
		showStatsHistogramsSQL   = "SHOW STATS_HISTOGRAMS"
		selectStatsHistogramsSQL = "SELECT TABLE_ID,FROM_UNIXTIME(VERSION DIV 262144 DIV 1000,'%Y-%m-%d %H:%i:%s') AS UPDATE_TIME,DISTINCT_COUNT FROM mysql.stats_histograms"
	)
	partitionIDs := make(map[string]map[string]map[string]int64, len(tables))
	rows, err := db.QueryContext(context.Background(), showStatsHistogramsSQL)
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", showStatsHistogramsSQL)
	}
	results, err := GetSpecifiedColumnValuesAndClose(rows, "DB_NAME", "TABLE_NAME", "PARTITION_NAME", "UPDATE_TIME", "DISTINCT_COUNT")
	if err != nil {
		return nil, errors.Annotatef(err, "sql: %s", showStatsHistogramsSQL)
	}
	type partitionInfo struct {
		dbName, tbName, partitionName string
	}
	saveMap := make(map[string]map[string]partitionInfo)
	for _, oneRow := range results {
		dbName, tbName, partitionName, updateTime, distinctCount := oneRow[0], oneRow[1], oneRow[2], oneRow[3], oneRow[4]
		if len(partitionName) == 0 {
			continue
		}
		if tbm, ok := tables[dbName]; ok {
			if _, ok = tbm[tbName]; ok {
				if _, ok = saveMap[updateTime]; !ok {
					saveMap[updateTime] = make(map[string]partitionInfo)
				}
				saveMap[updateTime][distinctCount] = partitionInfo{
					dbName:        dbName,
					tbName:        tbName,
					partitionName: partitionName,
				}
			}
		}
	}
	if len(saveMap) == 0 {
		return map[string]map[string]map[string]int64{}, nil
	}
	err = simpleQuery(db, selectStatsHistogramsSQL, func(rows *sql.Rows) error {
		var (
			tableID                   int64
			updateTime, distinctCount string
		)
		err2 := rows.Scan(&tableID, &updateTime, &distinctCount)
		if err2 != nil {
			return errors.Trace(err2)
		}
		if mpt, ok := saveMap[updateTime]; ok {
			if partition, ok := mpt[distinctCount]; ok {
				dbName, tbName, partitionName := partition.dbName, partition.tbName, partition.partitionName
				if _, ok := partitionIDs[dbName]; !ok {
					partitionIDs[dbName] = make(map[string]map[string]int64)
				}
				if _, ok := partitionIDs[dbName][tbName]; !ok {
					partitionIDs[dbName][tbName] = make(map[string]int64)
				}
				partitionIDs[dbName][tbName][partitionName] = tableID
			}
		}
		return nil
	})
	return partitionIDs, err
}

// GetDBInfo get model.DBInfos from database sql interface.
// We need table_id to check whether a region belongs to this table
func GetDBInfo(db *sql.Conn, tables map[string]map[string]struct{}) ([]*model.DBInfo, error) {
	const tableIDSQL = "SELECT TABLE_SCHEMA,TABLE_NAME,TIDB_TABLE_ID FROM INFORMATION_SCHEMA.TABLES ORDER BY TABLE_SCHEMA"

	schemas := make([]*model.DBInfo, 0, len(tables))
	var (
		tableSchema, tableName string
		tidbTableID            int64
	)
	partitionIDs, err := GetPartitionTableIDs(db, tables)
	if err != nil {
		return nil, err
	}
	err = simpleQuery(db, tableIDSQL, func(rows *sql.Rows) error {
		err2 := rows.Scan(&tableSchema, &tableName, &tidbTableID)
		if err2 != nil {
			return errors.Trace(err2)
		}
		if tbm, ok := tables[tableSchema]; !ok {
			return nil
		} else if _, ok = tbm[tableName]; !ok {
			return nil
		}
		last := len(schemas) - 1
		if last < 0 || schemas[last].Name.O != tableSchema {
			schemas = append(schemas, &model.DBInfo{
				Name:   model.CIStr{O: tableSchema},
				Tables: make([]*model.TableInfo, 0, len(tables[tableSchema])),
			})
			last++
		}
		var partition *model.PartitionInfo
		if tbm, ok := partitionIDs[tableSchema]; ok {
			if ptm, ok := tbm[tableName]; ok {
				partition = &model.PartitionInfo{Definitions: make([]model.PartitionDefinition, 0, len(ptm))}
				for partitionName, partitionID := range ptm {
					partition.Definitions = append(partition.Definitions, model.PartitionDefinition{
						ID:   partitionID,
						Name: model.CIStr{O: partitionName},
					})
				}
			}
		}
		schemas[last].Tables = append(schemas[last].Tables, &model.TableInfo{
			ID:        tidbTableID,
			Name:      model.CIStr{O: tableName},
			Partition: partition,
		})
		return nil
	})
	return schemas, err
}

// GetRegionInfos get region info including regionID, start key, end key from database sql interface.
// start key, end key includes information to help split table
func GetRegionInfos(db *sql.Conn) (*helper.RegionsInfo, error) {
	const tableRegionSQL = "SELECT REGION_ID,START_KEY,END_KEY FROM INFORMATION_SCHEMA.TIKV_REGION_STATUS ORDER BY START_KEY;"
	var (
		regionID         int64
		startKey, endKey string
	)
	regionsInfo := &helper.RegionsInfo{Regions: make([]helper.RegionInfo, 0)}
	err := simpleQuery(db, tableRegionSQL, func(rows *sql.Rows) error {
		err := rows.Scan(&regionID, &startKey, &endKey)
		if err != nil {
			return errors.Trace(err)
		}
		regionsInfo.Regions = append(regionsInfo.Regions, helper.RegionInfo{
			ID:       regionID,
			StartKey: startKey,
			EndKey:   endKey,
		})
		return nil
	})
	return regionsInfo, err
}
