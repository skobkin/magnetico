package persistence

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/url"
	"text/template"
	"time"
	"unicode/utf8"

	_ "github.com/lib/pq"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type postgresDatabase struct {
	conn *sql.DB
}

func makePostgresDatabase(url_ *url.URL) (Database, error) {
	db := new(postgresDatabase)

	var err error
	db.conn, err = sql.Open("postgres", url_.String())
	if err != nil {
		return nil, errors.Wrap(err, "sql.Open")
	}

	// > Open may just validate its arguments without creating a connection to the database. To
	// > verify that the data source Name is valid, call Ping.
	// https://golang.org/pkg/database/sql/#Open
	if err = db.conn.Ping(); err != nil {
		return nil, errors.Wrap(err, "sql.DB.Ping")
	}

	// https://github.com/mattn/go-sqlite3/issues/618
	db.conn.SetConnMaxLifetime(0) // https://golang.org/pkg/database/sql/#DB.SetConnMaxLifetime
	db.conn.SetMaxOpenConns(3)
	db.conn.SetMaxIdleConns(3)

	if err := db.setupDatabase(); err != nil {
		return nil, errors.Wrap(err, "setupDatabase")
	}

	return db, nil
}

func (db *postgresDatabase) Engine() databaseEngine {
	return Postgres
}

func (db *postgresDatabase) DoesTorrentExist(infoHash []byte) (bool, error) {
	rows, err := db.conn.Query("SELECT 1 FROM torrents WHERE info_hash = $1;", infoHash)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	exists := rows.Next()
	if rows.Err() != nil {
		return false, err
	}

	return exists, nil
}

func (db *postgresDatabase) AddNewTorrent(infoHash []byte, name string, files []File) error {
	if !utf8.ValidString(name) {
		zap.L().Warn(
			"Ignoring a torrent whose name is not UTF-8 compliant.",
			zap.ByteString("infoHash", infoHash),
			zap.Binary("name", []byte(name)),
		)

		return nil
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return errors.Wrap(err, "conn.Begin")
	}
	// If everything goes as planned and no error occurs, we will commit the transaction before
	// returning from the function so the tx.Rollback() call will fail, trying to rollback a
	// committed transaction. BUT, if an error occurs, we'll get our transaction rollback'ed, which
	// is nice.
	defer tx.Rollback()

	var totalSize uint64 = 0
	for _, file := range files {
		totalSize += uint64(file.Size)
	}

	// This is a workaround for a bug: the database will not accept total_size to be zero.
	if totalSize == 0 {
		zap.L().Debug("Ignoring a torrent whose total size is zero.")
		return nil
	}

	if exist, err := db.DoesTorrentExist(infoHash); exist || err != nil {
		return err
	}

	var lastInsertId int64

	err = tx.QueryRow(`
		INSERT INTO torrents (
			info_hash,
			name,
			total_size,
			discovered_on
		) VALUES ($1, $2, $3, $4)
		RETURNING id;
	`, infoHash, name, totalSize, time.Now().Unix()).Scan(&lastInsertId)
	if err != nil {
		return errors.Wrap(err, "tx.QueryRow (INSERT INTO torrents)")
	}

	for _, file := range files {
		if !utf8.ValidString(file.Path) {
			zap.L().Warn(
				"Ignoring a file whose path is not UTF-8 compliant.",
				zap.Binary("path", []byte(file.Path)),
			)

			// Returning nil so deferred tx.Rollback() will be called and transaction will be canceled.
			return nil
		}

		_, err = tx.Exec("INSERT INTO files (torrent_id, size, path) VALUES ($1, $2, $3);",
			lastInsertId, file.Size, file.Path,
		)
		if err != nil {
			return errors.Wrap(err, "tx.Exec (INSERT INTO files)")
		}
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "tx.Commit")
	}

	return nil
}

func (db *postgresDatabase) Close() error {
	return db.conn.Close()
}

func (db *postgresDatabase) GetNumberOfTorrents() (uint, error) {
	// Using estimated number of rows which can be queries much quicker
	// https://www.postgresql.org/message-id/568BF820.9060101%40comarch.com
	// https://wiki.postgresql.org/wiki/Count_estimate
	rows, err := db.conn.Query("SELECT reltuples::BIGINT AS estimate_count FROM pg_class WHERE relname='torrents';")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	if rows.Next() != true {
		return 0, fmt.Errorf("no rows returned from `SELECT reltuples::BIGINT AS estimate_count`")
	}

	// Returns int64: https://godoc.org/github.com/lib/pq#hdr-Data_Types
	var n *uint
	if err = rows.Scan(&n); err != nil {
		return 0, err
	}

	// If the database is empty (i.e. 0 entries in 'torrents') then the query will return nil.
	if n == nil {
		return 0, nil
	} else {
		return *n, nil
	}
}

func (db *postgresDatabase) QueryTorrents(
	query string,
	epoch int64,
	orderBy OrderingCriteria,
	ascending bool,
	limit uint,
	lastOrderedValue *float64,
	lastID *uint64,
) ([]TorrentMetadata, error) {
	err := fmt.Errorf("QueryTorrents is not supported yet by PostgreSQL backend")

	torrents := make([]TorrentMetadata, 0)

	return torrents, err
}

func (db *postgresDatabase) orderOn(orderBy OrderingCriteria) string {
	panic("Not implemented yet for PostgreSQL")
}

func (db *postgresDatabase) GetTorrent(infoHash []byte) (*TorrentMetadata, error) {
	rows, err := db.conn.Query(`
		SELECT
			info_hash,
			name,
			total_size,
			discovered_on,
			(SELECT COUNT(*) FROM files WHERE torrent_id = torrents.id) AS n_files
		FROM torrents
		WHERE info_hash = $1;`,
		infoHash,
	)
	defer db.closeRows(rows)
	if err != nil {
		return nil, err
	}

	if rows.Next() != true {
		return nil, nil
	}

	var tm TorrentMetadata
	if err = rows.Scan(&tm.InfoHash, &tm.Name, &tm.Size, &tm.DiscoveredOn, &tm.NFiles); err != nil {
		return nil, err
	}

	return &tm, nil
}

func (db *postgresDatabase) GetFiles(infoHash []byte) ([]File, error) {
	rows, err := db.conn.Query(
		"SELECT size, path FROM files, torrents WHERE files.torrent_id = torrents.id AND torrents.info_hash = $1;",
		infoHash)
	defer db.closeRows(rows)
	if err != nil {
		return nil, err
	}

	var files []File
	for rows.Next() {
		var file File
		if err = rows.Scan(&file.Size, &file.Path); err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	return files, nil
}

func (db *postgresDatabase) GetStatistics(from string, n uint) (*Statistics, error) {
	panic("Not implemented yet for PostgreSQL")
}

func (db *postgresDatabase) setupDatabase() error {
	tx, err := db.conn.Begin()
	if err != nil {
		return errors.Wrap(err, "sql.DB.Begin")
	}

	defer tx.Rollback()

	// Initial Setup for schema version 0:
	// FROZEN.
	_, err = tx.Exec(`
		-- Torrents ID sequence generator
		CREATE SEQUENCE IF NOT EXISTS seq_torrents_id;
		-- Files ID sequence generator
		CREATE SEQUENCE IF NOT EXISTS seq_files_id;

		CREATE TABLE IF NOT EXISTS torrents (
			id             INTEGER PRIMARY KEY DEFAULT nextval('seq_torrents_id'),
			info_hash      bytea NOT NULL UNIQUE,
			name           TEXT NOT NULL,
			total_size     BIGINT NOT NULL CHECK(total_size > 0),
			discovered_on  INTEGER NOT NULL CHECK(discovered_on > 0)
		);

		CREATE TABLE IF NOT EXISTS files (
			id          INTEGER PRIMARY KEY DEFAULT nextval('seq_files_id'),
			torrent_id  INTEGER REFERENCES torrents ON DELETE CASCADE ON UPDATE RESTRICT,
			size        BIGINT NOT NULL,
			path        TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS migrations (
		    schema_version		SMALLINT NOT NULL UNIQUE 
		);

		INSERT INTO migrations (schema_version) VALUES (0) ON CONFLICT DO NOTHING;
	`)
	if err != nil {
		return errors.Wrap(err, "sql.Tx.Exec (v0)")
	}

	// Get current schema version
	rows, err := tx.Query("SELECT MAX(schema_version) FROM migrations;")
	if err != nil {
		return errors.Wrap(err, "sql.Tx.Query (SELECT MAX(version) FROM migrations)")
	}
	defer db.closeRows(rows)

	var schemaVersion int
	if rows.Next() != true {
		return fmt.Errorf("sql.Rows.Next (SELECT MAX(version) FROM migrations): Query did not return any rows")
	}
	if err = rows.Scan(&schemaVersion); err != nil {
		return errors.Wrap(err, "sql.Rows.Scan (MAX(version))")
	}
	// If next line is removed we're getting error on sql.Tx.Commit: unexpected command tag SELECT
	// https://stackoverflow.com/questions/36295883/golang-postgres-commit-unknown-command-error/36866993#36866993
	db.closeRows(rows)

	switch schemaVersion {
	case 0: // FROZEN.
		zap.L().Warn("Updating (fake) database schema from 0 to 1...")
		_, err = tx.Exec(`INSERT INTO migrations (schema_version) VALUES (1);`)
		if err != nil {
			return errors.Wrap(err, "sql.Tx.Exec (v0 -> v1)")
		}
		//fallthrough
	}

	if err = tx.Commit(); err != nil {
		return errors.Wrap(err, "sql.Tx.Commit")
	}

	return nil
}

func (db *postgresDatabase) executeTemplate(text string, data interface{}, funcs template.FuncMap) string {
	t := template.Must(template.New("anon").Funcs(funcs).Parse(text))

	var buf bytes.Buffer
	err := t.Execute(&buf, data)
	if err != nil {
		panic(err.Error())
	}
	return buf.String()
}

func (db *postgresDatabase) closeRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		zap.L().Error("could not close row", zap.Error(err))
	}
}
