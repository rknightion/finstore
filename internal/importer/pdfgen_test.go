package importer_test

import "github.com/rknightion/finstore/internal/importer/importertest"

// The statement generator lives in importertest so the ledger's tests can use it too.
type (
	genRow       = importertest.Row
	genStatement = importertest.Statement
)

func newGen(opening int64, rows []genRow) *genStatement { return importertest.New(opening, rows) }
