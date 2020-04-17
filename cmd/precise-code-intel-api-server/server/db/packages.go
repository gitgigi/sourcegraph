package db

import (
	"context"

	"github.com/keegancsmith/sqlf"
)

func (db *dbImpl) GetPackage(scheme, name, version string) (Dump, bool, error) {
	query := `
		SELECT
			d.id,
			d.commit,
			d.root,
			d.visible_at_tip,
			d.uploaded_at,
			d.state,
			d.failure_summary,
			d.failure_stacktrace,
			d.started_at,
			d.finished_at,
			d.tracing_context,
			d.repository_id,
			d.indexer
		FROM lsif_packages p
		JOIN lsif_dumps d ON p.dump_id = d.id
		WHERE p.scheme = %s AND p.name = %s AND p.version = %s
		LIMIT 1
	`

	dump, err := scanDump(db.queryRow(context.Background(), sqlf.Sprintf(query, scheme, name, version)))
	if err != nil {
		return Dump{}, false, ignoreErrNoRows(err)
	}

	return dump, true, nil
}
