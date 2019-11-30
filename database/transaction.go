package database

import (
	"strings"

	"github.com/asdine/genji/engine"
	"github.com/asdine/genji/index"
	"github.com/asdine/genji/document"
	"github.com/pkg/errors"
)

var (
	separator            byte = 0x1F
	tableConfigStoreName      = "__genji.tables"
	indexStoreName            = "__genji.indexes"
	indexPrefix               = "i"
)

// Transaction represents a database transaction. It provides methods for managing the
// collection of tables and the transaction itself.
// Transaction is either read-only or read/write. Read-only can be used to read tables
// and read/write can be used to read, create, delete and modify tables.
type Transaction struct {
	db         *Database
	tx         engine.Transaction
	writable   bool
	tcfgStore  *tableConfigStore
	indexStore *indexStore
}

// Rollback the transaction. Can be used safely after commit.
func (tx *Transaction) Rollback() error {
	return tx.tx.Rollback()
}

// Commit the transaction.
func (tx *Transaction) Commit() error {
	return tx.tx.Commit()
}

// Writable indicates if the transaction is writable or not.
func (tx *Transaction) Writable() bool {
	return tx.writable
}

// Promote rollsback a read-only transaction and begins a read-write transaction transparently.
// It returns an error if the current transaction is already writable.
func (tx *Transaction) Promote() error {
	if tx.writable {
		return errors.New("can't promote a writable transaction")
	}

	err := tx.Rollback()
	if err != nil {
		return err
	}

	newTransaction, err := tx.db.Begin(true)
	if err != nil {
		return err
	}

	*tx = *newTransaction
	return nil
}

// CreateTable creates a table with the given name.
// If it already exists, returns ErrTableAlreadyExists.
func (tx Transaction) CreateTable(name string, cfg *TableConfig) error {
	if cfg == nil {
		cfg = new(TableConfig)
	}
	err := tx.tcfgStore.Insert(name, *cfg)
	if err != nil {
		return err
	}

	err = tx.tx.CreateStore(name)
	if err != nil {
		return errors.Wrapf(err, "failed to create table %q", name)
	}

	return nil
}

// GetTable returns a table by name. The table instance is only valid for the lifetime of the transaction.
func (tx Transaction) GetTable(name string) (*Table, error) {
	_, err := tx.tcfgStore.Get(name)
	if err != nil {
		return nil, err
	}

	s, err := tx.tx.Store(name)
	if err != nil {
		return nil, err
	}

	return &Table{
		tx:       &tx,
		Store:    s,
		name:     name,
		CfgStore: tx.tcfgStore,
	}, nil
}

// DropTable deletes a table from the database.
func (tx Transaction) DropTable(name string) error {
	err := tx.tcfgStore.Delete(name)
	if err != nil {
		return err
	}

	return tx.tx.DropStore(name)
}

// ListTables lists all the tables.
func (tx Transaction) ListTables() ([]string, error) {
	stores, err := tx.tx.ListStores("")
	if err != nil {
		return nil, err
	}

	tables := make([]string, 0, len(stores))
	idxPrefix := indexPrefix + string([]byte{separator})

	for _, st := range stores {
		if st == indexStoreName || st == tableConfigStoreName {
			continue
		}
		if strings.HasPrefix(st, idxPrefix) {
			continue
		}

		tables = append(tables, st)
	}

	return tables, nil
}

func buildIndexName(name string) string {
	var b strings.Builder
	b.WriteString(indexPrefix)
	b.WriteByte(separator)
	b.WriteString(name)

	return b.String()
}

// CreateIndex creates an index with the given name.
// If it already exists, returns ErrTableAlreadyExists.
func (tx Transaction) CreateIndex(opts index.Options) error {
	_, err := tx.GetTable(opts.TableName)
	if err != nil {
		return err
	}

	idxOpts := indexOptions{
		IndexName: opts.IndexName,
		TableName: opts.TableName,
		FieldName: opts.FieldName,
		Unique:    opts.Unique,
	}

	return tx.indexStore.Insert(idxOpts)
}

// GetIndex returns an index by name.
func (tx Transaction) GetIndex(name string) (*Index, error) {
	opts, err := tx.indexStore.Get(name)
	if err != nil {
		return nil, err
	}

	return &Index{
		Index: index.New(tx.tx, index.Options{
			IndexName: opts.IndexName,
			TableName: opts.TableName,
			FieldName: opts.FieldName,
			Unique:    opts.Unique,
		}),
		IndexName: opts.IndexName,
		TableName: opts.TableName,
		FieldName: opts.FieldName,
		Unique:    opts.Unique,
	}, nil
}

// DropIndex deletes an index from the database.
func (tx Transaction) DropIndex(name string) error {
	opts, err := tx.indexStore.Get(name)
	if err != nil {
		return err
	}
	err = tx.indexStore.Delete(name)
	if err != nil {
		return err
	}

	return index.New(tx.tx, index.Options{
		IndexName: opts.IndexName,
		TableName: opts.TableName,
		FieldName: opts.FieldName,
		Unique:    opts.Unique,
	}).Truncate()
}

// ReIndex truncates and recreates selected index from scratch.
func (tx Transaction) ReIndex(indexName string) error {
	idx, err := tx.GetIndex(indexName)
	if err != nil {
		return err
	}

	tb, err := tx.GetTable(idx.TableName)
	if err != nil {
		return err
	}

	err = idx.Truncate()
	if err != nil {
		return err
	}

	return tb.Iterate(func(r document.Document) error {
		f, err := r.GetValueByName(idx.FieldName)
		if err != nil {
			return err
		}

		return idx.Set(f.Value, r.(document.Keyer).Key())
	})
}

// ReIndexAll truncates and recreates all indexes of the database from scratch.
func (tx Transaction) ReIndexAll() error {
	return tx.indexStore.st.AscendGreaterOrEqual(nil, func(k, v []byte) error {
		var opts indexOptions
		err := opts.ScanRecord(document.EncodedRecord(v))
		if err != nil {
			return err
		}

		idx := index.New(tx.tx, index.Options{
			IndexName: opts.IndexName,
			TableName: opts.TableName,
			FieldName: opts.FieldName,
			Unique:    opts.Unique,
		})

		tb, err := tx.GetTable(opts.TableName)
		if err != nil {
			return err
		}

		err = idx.Truncate()
		if err != nil {
			return err
		}

		return tb.Iterate(func(r document.Document) error {
			f, err := r.GetValueByName(opts.FieldName)
			if err != nil {
				return err
			}

			return idx.Set(f.Value, r.(document.Keyer).Key())
		})
	})
}

func (tx *Transaction) getTableConfigStore() (*tableConfigStore, error) {
	st, err := tx.tx.Store(tableConfigStoreName)
	if err != nil {
		return nil, err
	}
	return &tableConfigStore{
		st: st,
	}, nil
}

func (tx *Transaction) getIndexStore() (*indexStore, error) {
	st, err := tx.tx.Store(indexStoreName)
	if err != nil {
		return nil, err
	}
	return &indexStore{
		st: st,
	}, nil
}
