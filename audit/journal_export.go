package audit

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// JournalExportFormat is the format of a JournalExport.
const JournalExportFormat = "bide.audit.journal-export.v1"

// A JournalExport is a run's journal as the bytes the journal stores for each record, in persisted
// order: the input a prover that holds no store (bide-audit prove and prove-absent) builds proofs
// from. Each record is carried verbatim (base64 in JSON), never re-encoded, so a proof built from an
// export commits to exactly the leaves a proof built from the store does, including the fields of
// a record this version does not know.
type JournalExport struct {
	Format  string   `json:"format"`  // JournalExportFormat
	RunID   string   `json:"run_id"`  // the run the journal is of
	Records [][]byte `json:"records"` // each record's stored bytes, in persisted order
}

func (JournalExport) artifactFormat() (string, string) { return "journal export", JournalExportFormat }

// ExportJournal returns runID's journal as a JournalExport.
func ExportJournal(ctx context.Context, store *agent.Journal, runID string) (JournalExport, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return JournalExport{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	out := JournalExport{Format: JournalExportFormat, RunID: runID, Records: make([][]byte, len(recs))}
	for i, r := range recs {
		b := r.Raw()
		if b == nil {
			return JournalExport{}, fmt.Errorf("audit: export journal %s: record %d (%q) has no stored bytes", runID, i, r.Name)
		}
		out.Records[i] = b
	}
	return out, nil
}

// Journal decodes the exported records, each keeping its stored bytes as its Raw, so the proof
// builders commit to the bytes exported. A redaction tombstone decodes as a redacted record. It
// errors (wrapping ErrFormat or ErrMalformed) if the export is not of JournalExportFormat or a
// record does not decode.
func (x JournalExport) Journal() ([]agent.Record, error) {
	if err := formatOf(x, x.Format); err != nil {
		return nil, err
	}
	recs := make([]agent.Record, len(x.Records))
	for i, b := range x.Records {
		name, err := recordName(b)
		if err != nil {
			return nil, fmt.Errorf("audit: journal export of run %s: record %d: %w", x.RunID, i, err)
		}
		r, err := agent.DecodeStoredRecord(x.RunID, name, b)
		if err != nil {
			return nil, fmt.Errorf("audit: journal export of run %s: record %d: %v: %w", x.RunID, i, err, ErrMalformed)
		}
		recs[i] = r
	}
	return recs, nil
}

// recordName reads the name a record's stored bytes carry, or "" for a redaction tombstone (whose
// name is its key, which an export does not carry).
func recordName(b []byte) (string, error) {
	if _, err := tombstoneLeafHash(b); err == nil {
		return "", nil
	}
	r, err := decodeRecordBytes(b)
	if err != nil {
		return "", err
	}
	return r.Name, nil
}
