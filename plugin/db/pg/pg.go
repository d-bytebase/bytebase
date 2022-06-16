package pg

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bytebase/bytebase/common/log"

	// embed will embeds the migration schema.
	_ "embed"

	// Import pg driver.
	// init() in pgx/v4/stdlib will register it's pgx driver
	_ "github.com/jackc/pgx/v4/stdlib"

	"github.com/bytebase/bytebase/plugin/db"
	"github.com/bytebase/bytebase/plugin/db/util"
	"go.uber.org/zap"
)

//go:embed pg_migration_schema.sql
var migrationSchema string

var (
	systemDatabases = map[string]bool{
		"template0": true,
		"template1": true,
	}
	ident                      = regexp.MustCompile(`(?i)^[a-z_][a-z0-9_$]*$`)
	createBytebaseDatabaseStmt = "CREATE DATABASE bytebase;"

	// driverName is the driver name that our driver dependence register, now is "pgx".
	driverName = "pgx"

	_ db.Driver              = (*Driver)(nil)
	_ util.MigrationExecutor = (*Driver)(nil)
)

func init() {
	db.Register(db.Postgres, newDriver)
}

// Driver is the Postgres driver.
type Driver struct {
	pgInstanceDir string
	connectionCtx db.ConnectionContext
	config        db.ConnectionConfig

	db           *sql.DB
	baseDSN      string
	databaseName string

	// strictDatabase should be used only if the user gives only a database instead of a whole instance to access.
	strictDatabase string
}

func newDriver(config db.DriverConfig) db.Driver {
	return &Driver{
		pgInstanceDir: config.PgInstanceDir,
	}
}

// Open opens a Postgres driver.
func (driver *Driver) Open(ctx context.Context, dbType db.Type, config db.ConnectionConfig, connCtx db.ConnectionContext) (db.Driver, error) {
	if (config.TLSConfig.SslCert == "" && config.TLSConfig.SslKey != "") ||
		(config.TLSConfig.SslCert != "" && config.TLSConfig.SslKey == "") {
		return nil, fmt.Errorf("ssl-cert and ssl-key must be both set or unset")
	}

	databaseName, dsn, err := guessDSN(
		config.Username,
		config.Password,
		config.Host,
		config.Port,
		config.Database,
		config.TLSConfig.SslCA,
		config.TLSConfig.SslCert,
		config.TLSConfig.SslKey,
	)
	if err != nil {
		return nil, err
	}
	if config.ReadOnly {
		dsn = fmt.Sprintf("%s default_transaction_read_only=true", dsn)
	}
	driver.databaseName = databaseName
	driver.baseDSN = dsn
	driver.connectionCtx = connCtx
	driver.config = config
	if config.StrictUseDb {
		driver.strictDatabase = config.Database
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	driver.db = db
	return driver, nil
}

// guessDSN will guess a valid DB connection and its database name.
func guessDSN(username, password, hostname, port, database, sslCA, sslCert, sslKey string) (string, string, error) {
	// dbname is guessed if not specified.
	m := map[string]string{
		"host":     hostname,
		"port":     port,
		"user":     username,
		"password": password,
	}

	if sslCA == "" {
		// We should use the default connection dsn without setting sslmode.
		// Some provider might still perform default SSL check at the server side so we
		// shouldn't disable sslmode at the client side.
		// m["sslmode"] = "disable"
	} else {
		m["sslmode"] = "verify-ca"
		m["sslrootcert"] = sslCA
		if sslCert != "" && sslKey != "" {
			m["sslcert"] = sslCert
			m["sslkey"] = sslKey
		}
	}
	var tokens []string
	for k, v := range m {
		if v != "" {
			tokens = append(tokens, fmt.Sprintf("%s=%s", k, v))
		}
	}
	dsn := strings.Join(tokens, " ")

	var guesses []string
	if database != "" {
		guesses = append(guesses, database)
	} else {
		// Guess default database postgres, template1.
		guesses = append(guesses, "")
		guesses = append(guesses, "bytebase")
		guesses = append(guesses, "postgres")
		guesses = append(guesses, "template1")
	}

	//  dsn+" dbname=bytebase"
	for _, guess := range guesses {
		guessDSN := dsn
		if guess != "" {
			guessDSN = fmt.Sprintf("%s dbname=%s", dsn, guess)
		}
		db, err := sql.Open(driverName, guessDSN)
		if err != nil {
			continue
		}
		defer db.Close()

		if err = db.Ping(); err != nil {
			continue
		}
		return guess, guessDSN, nil
	}

	if database != "" {
		return "", "", fmt.Errorf("cannot connecting %q, make sure the connection info is correct and the database exists", database)
	}
	return "", "", fmt.Errorf("cannot connecting instance, make sure the connection info is correct")
}

// Close closes the driver.
func (driver *Driver) Close(ctx context.Context) error {
	return driver.db.Close()
}

// Ping pings the database.
func (driver *Driver) Ping(ctx context.Context) error {
	return driver.db.PingContext(ctx)
}

// GetDbConnection gets a database connection.
func (driver *Driver) GetDbConnection(ctx context.Context, database string) (*sql.DB, error) {
	if err := driver.switchDatabase(database); err != nil {
		return nil, err
	}
	return driver.db, nil
}

// GetVersion gets the version of Postgres server.
func (driver *Driver) GetVersion(ctx context.Context) (string, error) {
	query := "SHOW server_version"
	versionRow, err := driver.db.QueryContext(ctx, query)
	if err != nil {
		return "", util.FormatErrorWithQuery(err, query)
	}
	defer versionRow.Close()

	var version string
	versionRow.Next()
	if err := versionRow.Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

// SyncSchema syncs the schema.
func (driver *Driver) SyncSchema(ctx context.Context) ([]*db.User, []*db.Schema, error) {
	excludedDatabaseList := map[string]bool{
		// Skip our internal "bytebase" database
		"bytebase": true,
		// Skip internal databases from cloud service providers
		// see https://github.com/bytebase/bytebase/issues/30
		// aws
		"rdsadmin": true,
		// gcp
		"cloudsql": true,
	}
	// Skip all system databases
	for k := range systemDatabases {
		excludedDatabaseList[k] = true
	}

	// Query user info
	userList, err := driver.getUserList(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Query db info
	databases, err := driver.getDatabases()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get databases: %s", err)
	}

	var schemaList []*db.Schema
	for _, database := range databases {
		dbName := database.name
		if _, ok := excludedDatabaseList[dbName]; ok {
			continue
		}

		var schema db.Schema
		schema.Name = dbName
		schema.CharacterSet = database.encoding
		schema.Collation = database.collate

		sqldb, err := driver.GetDbConnection(ctx, dbName)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get database connection for %q: %s", dbName, err)
		}
		txn, err := sqldb.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		defer txn.Rollback()

		// Index statements.
		indicesMap := make(map[string][]*indexSchema)
		indices, err := getIndices(txn)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get indices from database %q: %s", dbName, err)
		}
		for _, idx := range indices {
			key := fmt.Sprintf("%s.%s", idx.schemaName, idx.tableName)
			indicesMap[key] = append(indicesMap[key], idx)
		}

		// Table statements.
		tables, err := getPgTables(txn)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get tables from database %q: %s", dbName, err)
		}
		for _, tbl := range tables {
			var dbTable db.Table
			dbTable.Name = fmt.Sprintf("%s.%s", tbl.schemaName, tbl.name)
			dbTable.Type = "BASE TABLE"
			dbTable.Comment = tbl.comment
			dbTable.RowCount = tbl.rowCount
			dbTable.DataSize = tbl.tableSizeByte
			dbTable.IndexSize = tbl.indexSizeByte
			for _, col := range tbl.columns {
				var dbColumn db.Column
				dbColumn.Name = col.columnName
				dbColumn.Position = col.ordinalPosition
				dbColumn.Default = &col.columnDefault
				dbColumn.Type = col.dataType
				dbColumn.Nullable = col.isNullable
				dbColumn.Collation = col.collationName
				dbColumn.Comment = col.comment
				dbTable.ColumnList = append(dbTable.ColumnList, dbColumn)
			}
			indices := indicesMap[dbTable.Name]
			for _, idx := range indices {
				for i, colExp := range idx.columnExpressions {
					var dbIndex db.Index
					dbIndex.Name = idx.name
					dbIndex.Expression = colExp
					dbIndex.Position = i + 1
					dbIndex.Type = idx.methodType
					dbIndex.Unique = idx.unique
					dbIndex.Comment = idx.comment
					dbTable.IndexList = append(dbTable.IndexList, dbIndex)
				}
			}

			schema.TableList = append(schema.TableList, dbTable)
		}
		// View statements.
		views, err := getViews(txn)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get views from database %q: %s", dbName, err)
		}
		for _, view := range views {
			var dbView db.View
			dbView.Name = fmt.Sprintf("%s.%s", view.schemaName, view.name)
			// Postgres does not store
			dbView.CreatedTs = time.Now().Unix()
			dbView.Definition = view.definition
			dbView.Comment = view.comment

			schema.ViewList = append(schema.ViewList, dbView)
		}
		// Extensions.
		extensions, err := getExtensions(txn)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get extensions from database %q: %s", dbName, err)
		}
		schema.ExtensionList = extensions

		if err := txn.Commit(); err != nil {
			return nil, nil, err
		}

		schemaList = append(schemaList, &schema)
	}

	return userList, schemaList, err
}

func (driver *Driver) getUserList(ctx context.Context) ([]*db.User, error) {
	// Query user info
	query := `
		SELECT usename AS role_name,
			CASE
				 WHEN usesuper AND usecreatedb THEN
				 CAST('superuser, create database' AS pg_catalog.text)
				 WHEN usesuper THEN
					CAST('superuser' AS pg_catalog.text)
				 WHEN usecreatedb THEN
					CAST('create database' AS pg_catalog.text)
				 ELSE
					CAST('' AS pg_catalog.text)
			END role_attributes
		FROM pg_catalog.pg_user
		ORDER BY role_name
			`
	var userList []*db.User
	userRows, err := driver.db.QueryContext(ctx, query)
	if err != nil {
		return nil, util.FormatErrorWithQuery(err, query)
	}
	defer userRows.Close()

	for userRows.Next() {
		var role string
		var attr string
		if err := userRows.Scan(
			&role,
			&attr,
		); err != nil {
			return nil, err
		}

		userList = append(userList, &db.User{
			Name:  role,
			Grant: attr,
		})
	}
	return userList, nil
}

// Execute executes a SQL statement.
func (driver *Driver) Execute(ctx context.Context, statement string) error {
	var remainingStmts []string
	f := func(stmt string) error {
		stmt = strings.TrimLeft(stmt, " \t")
		// We don't use transaction for creating / altering databases in Postgres.
		// https://github.com/bytebase/bytebase/issues/202
		if strings.HasPrefix(stmt, "CREATE DATABASE ") {
			databases, err := driver.getDatabases()
			if err != nil {
				return err
			}
			databaseName, err := getDatabaseInCreateDatabaseStatement(stmt)
			if err != nil {
				return err
			}
			exist := false
			for _, database := range databases {
				if database.name == databaseName {
					exist = true
					break
				}
			}

			if !exist {
				if _, err := driver.db.ExecContext(ctx, stmt); err != nil {
					return err
				}
			}
		} else if strings.HasPrefix(stmt, "ALTER DATABASE") && strings.Contains(stmt, " OWNER TO ") {
			if _, err := driver.db.ExecContext(ctx, stmt); err != nil {
				return err
			}
		} else if strings.HasPrefix(stmt, "\\connect ") {
			// For the case of `\connect "dbname";`, we need to use GetDbConnection() instead of executing the statement.
			parts := strings.Split(stmt, `"`)
			if len(parts) != 3 {
				return fmt.Errorf("invalid statement %q", stmt)
			}
			_, err := driver.GetDbConnection(ctx, parts[1])
			return err
		} else {
			remainingStmts = append(remainingStmts, stmt)
		}
		return nil
	}
	sc := bufio.NewScanner(strings.NewReader(statement))
	if err := util.ApplyMultiStatements(sc, f); err != nil {
		return err
	}

	if len(remainingStmts) == 0 {
		return nil
	}

	tx, err := driver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	owner, err := driver.getCurrentDatabaseOwner(tx)
	if err != nil {
		return err
	}
	// Set the current transaction role to the database owner so that the owner of created database will be the same as the database owner.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL ROLE %s", owner)); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, strings.Join(remainingStmts, "\n")); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func getDatabaseInCreateDatabaseStatement(createDatabaseStatement string) (string, error) {
	raw := strings.TrimRight(createDatabaseStatement, ";")
	raw = strings.TrimPrefix(raw, "CREATE DATABASE")
	tokens := strings.Fields(raw)
	if len(tokens) == 0 {
		return "", fmt.Errorf("database name not found")
	}
	databaseName := strings.TrimLeft(tokens[0], `"`)
	databaseName = strings.TrimRight(databaseName, `"`)
	return databaseName, nil
}

func (driver *Driver) getCurrentDatabaseOwner(txn *sql.Tx) (string, error) {
	const query = `
		SELECT
			u.rolname
		FROM
			pg_roles AS u JOIN pg_database AS d ON (d.datdba = u.oid)
		WHERE
			d.datname = current_database();
		`
	rows, err := txn.Query(query)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var owner string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return "", err
		}
		owner = o
	}
	if owner == "" {
		return "", fmt.Errorf("Owner not found for the current database")
	}
	return owner, nil
}

// Query queries a SQL statement.
func (driver *Driver) Query(ctx context.Context, statement string, limit int) ([]interface{}, error) {
	return util.Query(ctx, driver.db, statement, limit)
}

// NeedsSetupMigration returns whether it needs to setup migration.
func (driver *Driver) NeedsSetupMigration(ctx context.Context) (bool, error) {
	// Don't use `bytebase` when user gives database instead of instance.
	if !driver.strictUseDb() {
		exist, err := driver.hasBytebaseDatabase(ctx)
		if err != nil {
			return false, err
		}
		if !exist {
			return true, nil
		}
		if err := driver.switchDatabase(db.BytebaseDatabase); err != nil {
			return false, err
		}
	}

	const query = `
		SELECT
		    1
		FROM information_schema.tables
		WHERE table_name = 'migration_history'
	`

	return util.NeedsSetupMigrationSchema(ctx, driver.db, query)
}

func (driver *Driver) hasBytebaseDatabase(ctx context.Context) (bool, error) {
	databases, err := driver.getDatabases()
	if err != nil {
		return false, err
	}
	exist := false
	for _, database := range databases {
		if database.name == db.BytebaseDatabase {
			exist = true
			break
		}
	}
	return exist, nil
}

// SetupMigrationIfNeeded sets up migration if needed.
func (driver *Driver) SetupMigrationIfNeeded(ctx context.Context) error {
	setup, err := driver.NeedsSetupMigration(ctx)
	if err != nil {
		return nil
	}

	if setup {
		log.Info("Bytebase migration schema not found, creating schema...",
			zap.String("environment", driver.connectionCtx.EnvironmentName),
			zap.String("database", driver.connectionCtx.InstanceName),
		)

		// Only try to create `bytebase` db when user provide an instance
		if !driver.strictUseDb() {
			exist, err := driver.hasBytebaseDatabase(ctx)
			if err != nil {
				log.Error("Failed to find bytebase database.",
					zap.Error(err),
					zap.String("environment", driver.connectionCtx.EnvironmentName),
					zap.String("database", driver.connectionCtx.InstanceName),
				)
				return fmt.Errorf("failed to find bytebase database error: %v", err)
			}

			if !exist {
				// Create `bytebase` database
				if _, err := driver.db.ExecContext(ctx, createBytebaseDatabaseStmt); err != nil {
					log.Error("Failed to create bytebase database.",
						zap.Error(err),
						zap.String("environment", driver.connectionCtx.EnvironmentName),
						zap.String("database", driver.connectionCtx.InstanceName),
					)
					return util.FormatErrorWithQuery(err, createBytebaseDatabaseStmt)
				}
			}

			if err := driver.switchDatabase(db.BytebaseDatabase); err != nil {
				log.Error("Failed to switch to bytebase database.",
					zap.Error(err),
					zap.String("environment", driver.connectionCtx.EnvironmentName),
					zap.String("database", driver.connectionCtx.InstanceName),
				)
				return fmt.Errorf("failed to switch to bytebase database error: %v", err)
			}
		}

		// Create `migration_history` table
		if _, err := driver.db.ExecContext(ctx, migrationSchema); err != nil {
			log.Error("Failed to initialize migration schema.",
				zap.Error(err),
				zap.String("environment", driver.connectionCtx.EnvironmentName),
				zap.String("database", driver.connectionCtx.InstanceName),
			)
			return util.FormatErrorWithQuery(err, migrationSchema)
		}
		log.Info("Successfully created migration schema.",
			zap.String("environment", driver.connectionCtx.EnvironmentName),
			zap.String("database", driver.connectionCtx.InstanceName),
		)
	}

	return nil
}

// FindLargestVersionSinceBaseline will find the largest version since last baseline or branch.
func (driver Driver) FindLargestVersionSinceBaseline(ctx context.Context, tx *sql.Tx, namespace string) (*string, error) {
	largestBaselineSequence, err := driver.FindLargestSequence(ctx, tx, namespace, true /* baseline */)
	if err != nil {
		return nil, err
	}
	const getLargestVersionSinceLastBaselineQuery = `
		SELECT MAX(version) FROM migration_history
		WHERE namespace = $1 AND sequence >= $2
	`
	row, err := tx.QueryContext(ctx, getLargestVersionSinceLastBaselineQuery,
		namespace, largestBaselineSequence,
	)
	if err != nil {
		return nil, util.FormatErrorWithQuery(err, getLargestVersionSinceLastBaselineQuery)
	}
	defer row.Close()

	var version sql.NullString
	row.Next()
	if err := row.Scan(&version); err != nil {
		return nil, err
	}

	if version.Valid {
		return &version.String, nil
	}

	return nil, nil
}

// FindLargestSequence will return the largest sequence number.
func (Driver) FindLargestSequence(ctx context.Context, tx *sql.Tx, namespace string, baseline bool) (int, error) {
	findLargestSequenceQuery := `
		SELECT MAX(sequence) FROM migration_history
		WHERE namespace = $1`
	if baseline {
		findLargestSequenceQuery = fmt.Sprintf("%s AND (type = '%s' OR type = '%s')", findLargestSequenceQuery, db.Baseline, db.Branch)
	}
	row, err := tx.QueryContext(ctx, findLargestSequenceQuery,
		namespace,
	)
	if err != nil {
		return -1, util.FormatErrorWithQuery(err, findLargestSequenceQuery)
	}
	defer row.Close()

	var sequence sql.NullInt32
	row.Next()
	if err := row.Scan(&sequence); err != nil {
		return -1, err
	}

	if !sequence.Valid {
		// Returns 0 if we haven't applied any migration for this namespace.
		return 0, nil
	}

	return int(sequence.Int32), nil
}

// InsertPendingHistory will insert the migration record with pending status and return the inserted ID.
func (Driver) InsertPendingHistory(ctx context.Context, tx *sql.Tx, sequence int, prevSchema string, m *db.MigrationInfo, storedVersion, statement string) (int64, error) {
	const insertHistoryQuery = `
	INSERT INTO migration_history (
		created_by,
		created_ts,
		updated_by,
		updated_ts,
		release_version,
		namespace,
		sequence,
		source,
		type,
		status,
		version,
		description,
		statement,
		` + `"schema",` + `
		schema_prev,
		execution_duration_ns,
		issue_id,
		payload
	)
	VALUES ($1, EXTRACT(epoch from NOW()), $2, EXTRACT(epoch from NOW()), $3, $4, $5, $6, $7, 'PENDING', $8, $9, $10, $11, $12, 0, $13, $14)
	RETURNING id
	`
	var insertedID int64
	if err := tx.QueryRowContext(ctx, insertHistoryQuery,
		m.Creator,
		m.Creator,
		m.ReleaseVersion,
		m.Namespace,
		sequence,
		m.Source,
		m.Type,
		storedVersion,
		m.Description,
		statement,
		prevSchema,
		prevSchema,
		m.IssueID,
		m.Payload,
	).Scan(&insertedID); err != nil {
		return 0, err
	}
	return insertedID, nil
}

// UpdateHistoryAsDone will update the migration record as done.
func (Driver) UpdateHistoryAsDone(ctx context.Context, tx *sql.Tx, migrationDurationNs int64, updatedSchema string, insertedID int64) error {
	const updateHistoryAsDoneQuery = `
	UPDATE
		migration_history
	SET
		status = 'DONE',
		execution_duration_ns = $1,
		"schema" = $2
	WHERE id = $3
	`
	_, err := tx.ExecContext(ctx, updateHistoryAsDoneQuery, migrationDurationNs, updatedSchema, insertedID)
	return err
}

// UpdateHistoryAsFailed will update the migration record as failed.
func (Driver) UpdateHistoryAsFailed(ctx context.Context, tx *sql.Tx, migrationDurationNs int64, insertedID int64) error {
	const updateHistoryAsFailedQuery = `
	UPDATE
		migration_history
	SET
		status = 'FAILED',
		execution_duration_ns = $1
	WHERE id = $2
	`
	_, err := tx.ExecContext(ctx, updateHistoryAsFailedQuery, migrationDurationNs, insertedID)
	return err
}

// ExecuteMigration will execute the migration.
func (driver *Driver) ExecuteMigration(ctx context.Context, m *db.MigrationInfo, statement string) (int64, string, error) {
	if driver.strictUseDb() {
		return util.ExecuteMigration(ctx, driver, m, statement, driver.strictDatabase)
	}
	return util.ExecuteMigration(ctx, driver, m, statement, db.BytebaseDatabase)
}

// FindMigrationHistoryList finds the migration history.
func (driver *Driver) FindMigrationHistoryList(ctx context.Context, find *db.MigrationHistoryFind) ([]*db.MigrationHistory, error) {
	baseQuery := `
	SELECT
		id,
		created_by,
		created_ts,
		updated_by,
		updated_ts,
		release_version,
		namespace,
		sequence,
		source,
		type,
		status,
		version,
		description,
		statement,
		` + `"schema",` + `
		schema_prev,
		execution_duration_ns,
		issue_id,
		payload
		FROM migration_history `
	paramNames, params := []string{}, []interface{}{}
	if v := find.ID; v != nil {
		paramNames, params = append(paramNames, "id"), append(params, *v)
	}
	if v := find.Database; v != nil {
		paramNames, params = append(paramNames, "namespace"), append(params, *v)
	}
	if v := find.Version; v != nil {
		// TODO(d): support semantic versioning.
		storedVersion, err := util.ToStoredVersion(false, *v, "")
		if err != nil {
			return nil, err
		}
		paramNames, params = append(paramNames, "version"), append(params, storedVersion)
	}
	if v := find.Source; v != nil {
		paramNames, params = append(paramNames, "source"), append(params, *v)
	}
	var query = baseQuery +
		db.FormatParamNameInNumberedPosition(paramNames) +
		`ORDER BY created_ts DESC`
	if v := find.Limit; v != nil {
		query += fmt.Sprintf(" LIMIT %d", *v)
	}

	database := db.BytebaseDatabase
	if driver.strictUseDb() {
		database = driver.strictDatabase
	}
	history, err := util.FindMigrationHistoryList(ctx, query, params, driver, database, find, baseQuery)
	// TODO(d): remove this block once all existing customers all migrated to semantic versioning.
	// Skip this backfill for bytebase's database "bb" with user "bb". We will use the one in pg_engine.go instead.
	isBytebaseDatabase := strings.Contains(driver.baseDSN, "user=bb") && strings.Contains(driver.baseDSN, "host=/tmp")
	if err != nil && !isBytebaseDatabase {
		if !strings.Contains(err.Error(), "invalid stored version") {
			return nil, err
		}
		if err := driver.updateMigrationHistoryStorageVersion(ctx); err != nil {
			return nil, err
		}
		return util.FindMigrationHistoryList(ctx, query, params, driver, db.BytebaseDatabase, find, baseQuery)
	}
	return history, err
}

func (driver *Driver) updateMigrationHistoryStorageVersion(ctx context.Context) error {
	var sqldb *sql.DB
	var err error
	if !driver.strictUseDb() {
		sqldb, err = driver.GetDbConnection(ctx, db.BytebaseDatabase)
	}
	if err != nil {
		return err
	}

	query := `SELECT id, version FROM migration_history`
	rows, err := sqldb.Query(query)
	if err != nil {
		return err
	}
	type ver struct {
		id      int
		version string
	}
	var vers []ver
	for rows.Next() {
		var v ver
		if err := rows.Scan(&v.id, &v.version); err != nil {
			return err
		}
		vers = append(vers, v)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	updateQuery := `
		UPDATE
			migration_history
		SET
			version = $1
		WHERE id = $2 AND version = $3
	`
	for _, v := range vers {
		if strings.HasPrefix(v.version, util.NonSemanticPrefix) {
			continue
		}
		newVersion := fmt.Sprintf("%s%s", util.NonSemanticPrefix, v.version)
		if _, err := sqldb.Exec(updateQuery, newVersion, v.id, v.version); err != nil {
			return err
		}
	}
	return nil
}

// Dump and restore

// Dump dumps the database.
func (driver *Driver) Dump(ctx context.Context, database string, out io.Writer, schemaOnly bool) (string, error) {
	// pg_dump -d dbName --schema-only+

	// Find all dumpable databases
	databases, err := driver.getDatabases()
	if err != nil {
		return "", fmt.Errorf("failed to get databases: %s", err)
	}

	var dumpableDbNames []string
	if database != "" {
		exist := false
		for _, n := range databases {
			if n.name == database {
				exist = true
				break
			}
		}
		if !exist {
			return "", fmt.Errorf("database %s not found", database)
		}
		dumpableDbNames = []string{database}
	} else {
		for _, n := range databases {
			if systemDatabases[n.name] {
				continue
			}
			dumpableDbNames = append(dumpableDbNames, n.name)
		}
	}

	for _, dbName := range dumpableDbNames {
		includeUseDatabase := len(dumpableDbNames) > 1
		if err := driver.dumpOneDatabaseWithPgDump(ctx, dbName, out, schemaOnly, includeUseDatabase); err != nil {
			return "", err
		}
	}

	return "", nil
}

// Restore restores a database.
func (driver *Driver) Restore(ctx context.Context, sc *bufio.Scanner) (err error) {
	txn, err := driver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer txn.Rollback()

	f := func(stmt string) error {
		if _, err := txn.Exec(stmt); err != nil {
			return err
		}
		return nil
	}

	if err := util.ApplyMultiStatements(sc, f); err != nil {
		return err
	}

	if err := txn.Commit(); err != nil {
		return err
	}

	return nil
}

// RestoreTx restores the database in the given transaction.
func (driver *Driver) RestoreTx(ctx context.Context, tx *sql.Tx, sc *bufio.Scanner) error {
	return fmt.Errorf("Unimplemented")
}

func (driver *Driver) dumpOneDatabaseWithPgDump(ctx context.Context, database string, out io.Writer, schemaOnly bool, includeUseDatabase bool) error {
	var args []string
	args = append(args, fmt.Sprintf("--username=%s", driver.config.Username))
	if driver.config.Password == "" {
		args = append(args, "--no-password")
	}
	args = append(args, fmt.Sprintf("--host=%s", driver.config.Host))
	args = append(args, fmt.Sprintf("--port=%s", driver.config.Port))
	if schemaOnly {
		args = append(args, "--schema-only")
	}
	args = append(args, "--inserts")
	args = append(args, "--use-set-session-authorization")
	args = append(args, database)
	pgDumpPath := filepath.Join(driver.pgInstanceDir, "bin", "pg_dump")
	cmd := exec.Command(pgDumpPath, args...)
	if driver.config.Password != "" {
		// Unlike MySQL, PostgreSQL does not support specifying commands in commands, we can do this by means of environment variables.
		cmd.Env = append(cmd.Env, fmt.Sprintf("PGPASSWORD=%s", driver.config.Password))
	}
	cmd.Stderr = os.Stderr
	r, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s := bufio.NewScanner(r)
	previousLineComment := false
	previousLineEmpty := false
	for s.Scan() {
		line := s.Text()
		// Skip "SET SESSION AUTHORIZATION" till we can support it.
		if strings.HasPrefix(line, "SET SESSION AUTHORIZATION ") {
			continue
		}
		// Skip comment lines.
		if strings.HasPrefix(line, "--") {
			previousLineComment = true
			continue
		} else {
			if previousLineComment && line == "" {
				previousLineComment = false
				continue
			}
		}
		previousLineComment = false
		// Skip extra empty lines.
		if line == "" {
			if previousLineEmpty {
				continue
			}
			previousLineEmpty = true
		} else {
			previousLineEmpty = false
		}

		if _, err := io.WriteString(out, line); err != nil {
			return err
		}
		if _, err := io.WriteString(out, "\n"); err != nil {
			return err
		}
	}
	if s.Err() != nil {
		log.Warn(s.Err().Error())
	}
	if err := cmd.Wait(); err != nil {
		return err
	}
	return nil
}

func (driver *Driver) switchDatabase(dbName string) error {
	if driver.db != nil {
		if err := driver.db.Close(); err != nil {
			return err
		}
	}

	dsn := driver.baseDSN + " dbname=" + dbName
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return err
	}
	driver.db = db
	driver.databaseName = dbName
	return nil
}

// getDatabases gets all databases of an instance.
func (driver *Driver) getDatabases() ([]*pgDatabaseSchema, error) {
	var dbs []*pgDatabaseSchema
	rows, err := driver.db.Query("SELECT datname, pg_encoding_to_char(encoding), datcollate FROM pg_database;")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var d pgDatabaseSchema
		if err := rows.Scan(&d.name, &d.encoding, &d.collate); err != nil {
			return nil, err
		}
		dbs = append(dbs, &d)
	}
	return dbs, nil
}

func (driver *Driver) strictUseDb() bool {
	return len(driver.strictDatabase) != 0
}

// pgDatabaseSchema describes a pg database schema.
type pgDatabaseSchema struct {
	name     string
	encoding string
	collate  string
}

// tableSchema describes the schema of a pg table.
type tableSchema struct {
	schemaName    string
	name          string
	tableowner    string
	comment       string
	rowCount      int64
	tableSizeByte int64
	indexSizeByte int64

	columns     []*columnSchema
	constraints []*tableConstraint
}

// columnSchema describes the schema of a pg table column.
type columnSchema struct {
	columnName             string
	dataType               string
	ordinalPosition        int
	characterMaximumLength string
	columnDefault          string
	isNullable             bool
	collationName          string
	comment                string
}

// tableConstraint describes constraint schema of a pg table.
type tableConstraint struct {
	name       string
	schemaName string
	tableName  string
	constraint string
}

// viewSchema describes the schema of a pg view.
type viewSchema struct {
	schemaName string
	name       string
	definition string
	comment    string
}

// indexSchema describes the schema of a pg index.
type indexSchema struct {
	schemaName string
	name       string
	tableName  string
	statement  string
	unique     bool
	// methodType such as btree.
	methodType        string
	columnExpressions []string
	comment           string
}

// Statement returns the statement of a table column.
func (c *columnSchema) Statement() string {
	s := fmt.Sprintf("%s %s", quoteIdentifier(c.columnName), c.dataType)
	if c.characterMaximumLength != "" {
		s += fmt.Sprintf("(%s)", c.characterMaximumLength)
	}
	if !c.isNullable {
		s += " NOT NULL"
	}
	if c.columnDefault != "" {
		s += fmt.Sprintf(" DEFAULT %s", c.columnDefault)
	}
	return s
}

// Statement returns the create statement of a table constraint.
func (c *tableConstraint) Statement() string {
	return fmt.Sprintf(""+
		`ALTER TABLE ONLY "%s"."%s"\n`+
		`    ADD CONSTRAINT %s %s;\n`,
		c.schemaName, c.tableName, c.name, c.constraint)
}

// Statement returns the create statement of a view.
func (v *viewSchema) Statement() string {
	return fmt.Sprintf(""+
		"--\n"+
		"-- View structure for %s.%s\n"+
		"--\n"+
		"CREATE VIEW %s.%s AS\n%s\n\n",
		v.schemaName, v.name, v.schemaName, v.name, v.definition)
}

// Statement returns the create statement of an index.
func (idx indexSchema) Statement() string {
	return fmt.Sprintf(""+
		"--\n"+
		"-- Index structure for %s.%s\n"+
		"--\n"+
		"%s;\n\n",
		idx.schemaName, idx.name, idx.statement)
}

// getTables gets all tables of a database.
func getPgTables(txn *sql.Tx) ([]*tableSchema, error) {
	constraints, err := getTableConstraints(txn)
	if err != nil {
		return nil, fmt.Errorf("getTableConstraints() got error: %v", err)
	}

	var tables []*tableSchema
	query := "" +
		"SELECT tbl.schemaname, tbl.tablename, tbl.tableowner, pg_table_size(c.oid), pg_indexes_size(c.oid) " +
		"FROM pg_catalog.pg_tables tbl, pg_catalog.pg_class c " +
		"WHERE schemaname NOT IN ('pg_catalog', 'information_schema') AND tbl.schemaname=c.relnamespace::regnamespace::text AND tbl.tablename = c.relname;"
	rows, err := txn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var tbl tableSchema
		var schemaname, tablename, tableowner string
		var tableSizeByte, indexSizeByte int64
		if err := rows.Scan(&schemaname, &tablename, &tableowner, &tableSizeByte, &indexSizeByte); err != nil {
			return nil, err
		}
		tbl.schemaName = quoteIdentifier(schemaname)
		tbl.name = quoteIdentifier(tablename)
		tbl.tableowner = tableowner
		tbl.tableSizeByte = tableSizeByte
		tbl.indexSizeByte = indexSizeByte

		tables = append(tables, &tbl)
	}

	for _, tbl := range tables {
		if err := getTable(txn, tbl); err != nil {
			return nil, fmt.Errorf("getTable(%q, %q) got error %v", tbl.schemaName, tbl.name, err)
		}
		columns, err := getTableColumns(txn, tbl.schemaName, tbl.name)
		if err != nil {
			return nil, fmt.Errorf("getTableColumns(%q, %q) got error %v", tbl.schemaName, tbl.name, err)
		}
		tbl.columns = columns

		key := fmt.Sprintf("%s.%s", tbl.schemaName, tbl.name)
		tbl.constraints = constraints[key]
	}
	return tables, nil
}

func getTable(txn *sql.Tx, tbl *tableSchema) error {
	countQuery := fmt.Sprintf(`SELECT COUNT(1) FROM "%s"."%s";`, tbl.schemaName, tbl.name)
	rows, err := txn.Query(countQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if err := rows.Scan(&tbl.rowCount); err != nil {
			return err
		}
	}

	commentQuery := fmt.Sprintf(`SELECT obj_description('"%s"."%s"'::regclass);`, tbl.schemaName, tbl.name)
	crows, err := txn.Query(commentQuery)
	if err != nil {
		return err
	}
	defer crows.Close()

	for crows.Next() {
		var comment sql.NullString
		if err := crows.Scan(&comment); err != nil {
			return err
		}
		tbl.comment = comment.String
	}
	return nil
}

// getTableColumns gets the columns of a table.
func getTableColumns(txn *sql.Tx, schemaName, tableName string) ([]*columnSchema, error) {
	query := `
	SELECT
		cols.column_name,
		cols.data_type,
		cols.ordinal_position,
		cols.character_maximum_length,
		cols.column_default,
		cols.is_nullable,
		cols.collation_name,
		cols.udt_schema,
		cols.udt_name,
		(
			SELECT
					pg_catalog.col_description(c.oid, cols.ordinal_position::int)
			FROM pg_catalog.pg_class c
			WHERE
					c.oid     = (SELECT cols.table_name::regclass::oid) AND
					cols.table_schema=c.relnamespace::regnamespace::text AND
					cols.table_name = c.relname
		) as column_comment
	FROM INFORMATION_SCHEMA.COLUMNS AS cols
	WHERE table_schema=$1 AND table_name=$2;`
	rows, err := txn.Query(query, schemaName, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []*columnSchema
	for rows.Next() {
		var columnName, dataType, isNullable string
		var characterMaximumLength, columnDefault, collationName, udtSchema, udtName, comment sql.NullString
		var ordinalPosition int
		if err := rows.Scan(&columnName, &dataType, &ordinalPosition, &characterMaximumLength, &columnDefault, &isNullable, &collationName, &udtSchema, &udtName, &comment); err != nil {
			return nil, err
		}
		isNullBool, err := convertBoolFromYesNo(isNullable)
		if err != nil {
			return nil, err
		}
		c := columnSchema{
			columnName:             columnName,
			dataType:               dataType,
			ordinalPosition:        ordinalPosition,
			characterMaximumLength: characterMaximumLength.String,
			columnDefault:          columnDefault.String,
			isNullable:             isNullBool,
			collationName:          collationName.String,
			comment:                comment.String,
		}
		switch dataType {
		case "USER-DEFINED":
			c.dataType = fmt.Sprintf("%s.%s", udtSchema.String, udtName.String)
		case "ARRAY":
			c.dataType = udtName.String
		}
		columns = append(columns, &c)
	}
	return columns, nil
}

func convertBoolFromYesNo(s string) (bool, error) {
	switch s {
	case "YES":
		return true, nil
	case "NO":
		return false, nil
	default:
		return false, fmt.Errorf("unrecognized isNullable type %q", s)
	}
}

// getTableConstraints gets all table constraints of a database.
func getTableConstraints(txn *sql.Tx) (map[string][]*tableConstraint, error) {
	query := "" +
		"SELECT n.nspname, conrelid::regclass, conname, pg_get_constraintdef(c.oid) " +
		"FROM pg_constraint c " +
		"JOIN pg_namespace n ON n.oid = c.connamespace " +
		"WHERE n.nspname NOT IN ('pg_catalog', 'information_schema');"
	ret := make(map[string][]*tableConstraint)
	rows, err := txn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var constraint tableConstraint
		if err := rows.Scan(&constraint.schemaName, &constraint.tableName, &constraint.name, &constraint.constraint); err != nil {
			return nil, err
		}
		if strings.Contains(constraint.tableName, ".") {
			constraint.tableName = constraint.tableName[1+strings.Index(constraint.tableName, "."):]
		}
		constraint.schemaName, constraint.tableName, constraint.name = quoteIdentifier(constraint.schemaName), quoteIdentifier(constraint.tableName), quoteIdentifier(constraint.name)
		key := fmt.Sprintf("%s.%s", constraint.schemaName, constraint.tableName)
		ret[key] = append(ret[key], &constraint)
	}
	return ret, nil
}

// getViews gets all views of a database.
func getViews(txn *sql.Tx) ([]*viewSchema, error) {
	query := "" +
		"SELECT table_schema, table_name, view_definition FROM information_schema.views " +
		"WHERE table_schema NOT IN ('pg_catalog', 'information_schema');"
	var views []*viewSchema
	rows, err := txn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var view viewSchema
		var def sql.NullString
		if err := rows.Scan(&view.schemaName, &view.name, &def); err != nil {
			return nil, err
		}
		// Return error on NULL view definition.
		// https://github.com/bytebase/bytebase/issues/343
		if !def.Valid {
			return nil, fmt.Errorf("schema %q view %q has empty definition; please check whether proper privileges have been granted to Bytebase", view.schemaName, view.name)
		}
		view.schemaName, view.name, view.definition = quoteIdentifier(view.schemaName), quoteIdentifier(view.name), def.String
		views = append(views, &view)
	}

	for _, view := range views {
		if err = getView(txn, view); err != nil {
			return nil, fmt.Errorf("getPgView(%q, %q) got error %v", view.schemaName, view.name, err)
		}
	}
	return views, nil
}

// getView gets the schema of a view.
func getView(txn *sql.Tx, view *viewSchema) error {
	query := fmt.Sprintf(`SELECT obj_description('"%s"."%s"'::regclass);`, view.schemaName, view.name)
	rows, err := txn.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var comment sql.NullString
		if err := rows.Scan(&comment); err != nil {
			return err
		}
		view.comment = comment.String
	}
	return nil
}

func getExtensions(txn *sql.Tx) ([]db.Extension, error) {
	query := "" +
		"SELECT e.extname, e.extversion, n.nspname, c.description " +
		"FROM pg_catalog.pg_extension e " +
		"LEFT JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace " +
		"LEFT JOIN pg_catalog.pg_description c ON c.objoid = e.oid AND c.classoid = 'pg_catalog.pg_extension'::pg_catalog.regclass " +
		"WHERE n.nspname != 'pg_catalog';"

	var extensions []db.Extension
	rows, err := txn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var e db.Extension
		if err := rows.Scan(&e.Name, &e.Version, &e.Schema, &e.Description); err != nil {
			return nil, err
		}
		extensions = append(extensions, e)
	}

	return extensions, nil
}

// getIndices gets all indices of a database.
func getIndices(txn *sql.Tx) ([]*indexSchema, error) {
	query := "" +
		"SELECT schemaname, tablename, indexname, indexdef " +
		"FROM pg_indexes WHERE schemaname NOT IN ('pg_catalog', 'information_schema');"

	var indices []*indexSchema
	rows, err := txn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var idx indexSchema
		if err := rows.Scan(&idx.schemaName, &idx.tableName, &idx.name, &idx.statement); err != nil {
			return nil, err
		}
		idx.schemaName, idx.tableName, idx.name = quoteIdentifier(idx.schemaName), quoteIdentifier(idx.tableName), quoteIdentifier(idx.name)
		idx.unique = strings.Contains(idx.statement, " UNIQUE INDEX ")
		idx.methodType = getIndexMethodType(idx.statement)
		idx.columnExpressions, err = getIndexColumnExpressions(idx.statement)
		if err != nil {
			return nil, err
		}
		indices = append(indices, &idx)
	}

	for _, idx := range indices {
		if err = getIndex(txn, idx); err != nil {
			return nil, fmt.Errorf("getIndex(%q, %q) got error %v", idx.schemaName, idx.name, err)
		}
	}

	return indices, nil
}

func getIndex(txn *sql.Tx, idx *indexSchema) error {
	commentQuery := fmt.Sprintf(`SELECT obj_description('"%s"."%s"'::regclass);`, idx.schemaName, idx.name)
	crows, err := txn.Query(commentQuery)
	if err != nil {
		return err
	}
	defer crows.Close()

	for crows.Next() {
		var comment sql.NullString
		if err := crows.Scan(&comment); err != nil {
			return err
		}
		idx.comment = comment.String
	}
	return nil
}

func getIndexMethodType(stmt string) string {
	re := regexp.MustCompile(`USING (\w+) `)
	matches := re.FindStringSubmatch(stmt)
	if len(matches) == 0 {
		return ""
	}
	return matches[1]
}

func getIndexColumnExpressions(stmt string) ([]string, error) {
	rc := regexp.MustCompile(`\((.*)\)`)
	rm := rc.FindStringSubmatch(stmt)
	if len(rm) == 0 {
		return nil, fmt.Errorf("invalid index statement: %q", stmt)
	}
	columnStmt := rm[1]

	var cols []string
	re := regexp.MustCompile(`\(\(.*\)\)`)
	for {
		if len(columnStmt) == 0 {
			break
		}
		// Get a token
		token := ""
		// Expression has format of "((exp))".
		if strings.HasPrefix(columnStmt, "((") {
			token = re.FindString(columnStmt)
		} else {
			i := strings.Index(columnStmt, ",")
			if i < 0 {
				token = columnStmt
			} else {
				token = columnStmt[:i]
			}
		}
		// Strip token
		if len(token) == 0 {
			return nil, fmt.Errorf("invalid index statement: %q", stmt)
		}
		columnStmt = columnStmt[len(token):]
		cols = append(cols, strings.TrimSpace(token))

		// Trim space and remove a comma to prepare for the next tokenization.
		columnStmt = strings.TrimSpace(columnStmt)
		if len(columnStmt) > 0 && columnStmt[0] == ',' {
			columnStmt = columnStmt[1:]
		}
		columnStmt = strings.TrimSpace(columnStmt)
	}

	return cols, nil
}

// quoteIdentifier will quote identifiers including keywords, capital characters, or special characters.
func quoteIdentifier(s string) string {
	quote := false
	if reserved[strings.ToUpper(s)] {
		quote = true
	}
	if !ident.MatchString(s) {
		quote = true
	}
	if quote {
		return fmt.Sprintf("\"%s\"", strings.ReplaceAll(s, "\"", "\"\""))
	}
	return s

}
