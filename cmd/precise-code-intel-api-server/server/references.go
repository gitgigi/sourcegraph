package server

import (
	"context"
	"fmt"

	"github.com/sourcegraph/sourcegraph/cmd/precise-code-intel-api-server/server/bundles"
	"github.com/sourcegraph/sourcegraph/cmd/precise-code-intel-api-server/server/db"
)

func (s *Server) references(repositoryID int, commit string, limit int, cursor Cursor) ([]ResolvedLocation, Cursor, bool, error) {
	rpr := &ReferencePageResolver{
		db:                  s.db,
		bundleManagerClient: s.bundleManagerClient,
		repositoryID:        repositoryID,
		commit:              commit,
		limit:               limit,
	}

	return rpr.resolvePage(cursor)
}

type ReferencePageResolver struct {
	db                  db.DB
	bundleManagerClient bundles.BundleManagerClient
	repositoryID        int
	commit              string
	remoteDumpLimit     int
	limit               int
}

func (s *ReferencePageResolver) resolvePage(cursor Cursor) (locations []ResolvedLocation, newCursor Cursor, hasNewCursor bool, err error) {
	for s.limit > 0 {
		var batch []ResolvedLocation
		batch, newCursor, hasNewCursor, err = s.dispatchCursorHandler(cursor)
		locations = append(locations, batch...)
		if err != nil || !hasNewCursor {
			break
		}
	}

	return
}

func (s *ReferencePageResolver) dispatchCursorHandler(cursor Cursor) ([]ResolvedLocation, Cursor, bool, error) {
	fns := map[string]func(Cursor) ([]ResolvedLocation, Cursor, bool, error){
		"same-dump":           s.handleSameDumpCursor,
		"definition-monikers": s.handleDefinitionMonikersCursor,
		"same-repo":           s.handleSameRepoCursor,
		"remote-repo":         s.handleRemoteRepoCursor,
	}

	fn, exists := fns[cursor.Phase]
	if !exists {
		return nil, Cursor{}, false, fmt.Errorf("unknown cursor phase %s", cursor.Phase)
	}

	return fn(cursor)
}

func (s *ReferencePageResolver) handleSameDumpCursor(cursor Cursor) ([]ResolvedLocation, Cursor, bool, error) {
	dump, exists, err := s.db.GetDumpByID(context.Background(), cursor.DumpID)
	if err != nil {
		return nil, Cursor{}, false, err
	}
	if !exists {
		return nil, Cursor{}, false, ErrMissingDump
	}
	bundleClient := s.bundleManagerClient.BundleClient(dump.ID)

	locations, err := bundleClient.References(context.Background(), cursor.Path, cursor.Line, cursor.Character)
	if err != nil {
		return nil, Cursor{}, false, err
	}

	// Search the references table of the current dump. This search is necessary because
	// we want a 'Find References' operation on a reference to also return references to
	// the governing definition, and those may not be fully linked in the LSIF data. This
	// method returns a cursor if there are reference rows remaining for a subsequent page.
	for _, moniker := range cursor.Monikers {
		results, _, err := bundleClient.MonikerResults(context.Background(), "reference", moniker.Scheme, moniker.Identifier, 0, 0)
		if err != nil {
			return nil, Cursor{}, false, err
		}

		// TODO - deduplicate here
		locations = append(locations, results...)
	}

	// TODO - make an ordered location set (also in bundle manager)
	resolvedLocations := resolveLocationsWithDump(dump, sliceLocations(locations, cursor.SkipResults, cursor.SkipResults+s.limit))

	if newOffset := cursor.SkipResults + s.limit; newOffset <= len(locations) {
		newCursor := Cursor{
			Phase:       cursor.Phase,
			DumpID:      cursor.DumpID,
			Path:        cursor.Path,
			Line:        cursor.Line,
			Character:   cursor.Character,
			Monikers:    cursor.Monikers,
			SkipResults: newOffset,
		}
		return resolvedLocations, newCursor, true, nil
	}

	newCursor := Cursor{
		DumpID:      cursor.DumpID,
		Phase:       "definition-monikers",
		Path:        cursor.Path,
		Line:        cursor.Line,
		Character:   cursor.Character,
		Monikers:    cursor.Monikers,
		SkipResults: 0,
	}
	return resolvedLocations, newCursor, true, nil
}

func (s *ReferencePageResolver) handleDefinitionMonikersCursor(cursor Cursor) ([]ResolvedLocation, Cursor, bool, error) {
	var hasNextPhaseCursor = false
	var nextPhaseCursor Cursor
	for _, moniker := range cursor.Monikers {
		if moniker.PackageInformationID == "" {
			continue
		}

		packageInformation, err := s.bundleManagerClient.BundleClient(cursor.DumpID).PackageInformation(context.Background(), cursor.Path, moniker.PackageInformationID)
		if err != nil {
			return nil, Cursor{}, false, err
		}

		hasNextPhaseCursor = true
		nextPhaseCursor = Cursor{
			DumpID:                 cursor.DumpID,
			Phase:                  "same-repo",
			Scheme:                 moniker.Scheme,
			Identifier:             moniker.Identifier,
			Name:                   packageInformation.Name,
			Version:                packageInformation.Version,
			DumpIDs:                nil,
			TotalDumpsWhenBatching: 0,
			SkipDumpsWhenBatching:  0,
			SkipDumpsInBatch:       0,
			SkipResultsInDump:      0,
		}
		break
	}

	for _, moniker := range cursor.Monikers {
		if moniker.Kind != "import" {
			continue
		}

		locations, count, err := lookupMoniker(s.db, s.bundleManagerClient, cursor.DumpID, cursor.Path, "reference", moniker, s.limit, cursor.SkipResults)
		if err != nil {
			return nil, Cursor{}, false, err
		}
		if len(locations) == 0 {
			continue
		}

		if newOffset := cursor.SkipResults + len(locations); newOffset < count {
			newCursor := Cursor{
				Phase:       cursor.Phase,
				DumpID:      cursor.DumpID,
				Path:        cursor.Path,
				Monikers:    cursor.Monikers,
				SkipResults: newOffset,
			}
			return locations, newCursor, true, nil
		}

		return locations, nextPhaseCursor, hasNextPhaseCursor, nil
	}

	return nil, nextPhaseCursor, hasNextPhaseCursor, nil

}

func (s *ReferencePageResolver) handleSameRepoCursor(cursor Cursor) ([]ResolvedLocation, Cursor, bool, error) {
	locations, newCursor, hasNewCursor, err := s.fooborp(cursor, func() (int, *db.ReferencePager, error) {
		return s.db.SameRepoPager(context.Background(), s.repositoryID, s.commit, cursor.Scheme, cursor.Name, cursor.Version, s.remoteDumpLimit)
	})
	if err != nil || hasNewCursor {
		return locations, newCursor, hasNewCursor, err
	}

	newCursor = Cursor{
		DumpID:                 cursor.DumpID,
		Phase:                  "remote-repo",
		Scheme:                 cursor.Scheme,
		Identifier:             cursor.Identifier,
		Name:                   cursor.Name,
		Version:                cursor.Version,
		DumpIDs:                nil,
		TotalDumpsWhenBatching: 0,
		SkipDumpsWhenBatching:  0,
		SkipDumpsInBatch:       0,
		SkipResultsInDump:      0,
	}
	return locations, newCursor, true, nil
}

func (s *ReferencePageResolver) handleRemoteRepoCursor(cursor Cursor) ([]ResolvedLocation, Cursor, bool, error) {
	return s.fooborp(cursor, func() (int, *db.ReferencePager, error) {
		return s.db.PackageReferencePager(context.Background(), cursor.Scheme, cursor.Name, cursor.Version, s.repositoryID, s.remoteDumpLimit)
	})
}

//
//
//
//

// TODO - rename
func (s *ReferencePageResolver) fooborp(cursor Cursor, q func() (int, *db.ReferencePager, error)) ([]ResolvedLocation, Cursor, bool, error) {
	dumpID := cursor.DumpID
	scheme := cursor.Scheme
	identifier := cursor.Identifier
	limit := s.limit

	if len(cursor.DumpIDs) == 0 {
		totalCount, pager, err := q()
		if err != nil {
			return nil, Cursor{}, false, err
		}

		var refs []db.Reference
		identifier := cursor.Identifier
		offset := cursor.SkipDumpsWhenBatching
		limit := s.remoteDumpLimit
		newOffset := offset

		for len(refs) < limit && newOffset < totalCount {
			page, err := pager.PageFromOffset(newOffset)
			if err != nil {
				return nil, Cursor{}, false, pager.CloseTx(err)
			}

			if len(page) == 0 {
				// Shouldn't happen, but just in case of a bug we
				// don't want this to throw up into an infinite loop.
				break
			}

			filtered, scanned := applyBloomFilter(page, identifier, limit-len(refs))
			refs = append(refs, filtered...)
			newOffset += scanned
		}

		packageRefs := refs

		var dumpIDs []int
		for _, ref := range packageRefs {
			dumpIDs = append(dumpIDs, ref.DumpID)
		}

		cursor.DumpIDs = dumpIDs
		cursor.SkipDumpsWhenBatching = newOffset
		cursor.TotalDumpsWhenBatching = totalCount

		if err := pager.CloseTx(nil); err != nil {
			return nil, Cursor{}, false, err
		}
	}

	for i, batchDumpID := range cursor.DumpIDs {
		// Skip the remote reference that show up for ourselves - we've already gathered
		// these in the previous step of the references query.
		if i < cursor.SkipDumpsInBatch || batchDumpID == dumpID {
			continue
		}

		dump, exists, err := s.db.GetDumpByID(context.Background(), cursor.DumpID)
		if err != nil {
			return nil, Cursor{}, false, err
		}
		if !exists {
			continue
		}
		bundleClient := s.bundleManagerClient.BundleClient(dump.ID)

		results, count, err := bundleClient.MonikerResults(context.Background(), "reference", scheme, identifier, limit, cursor.SkipResultsInDump)
		if err != nil {
			return nil, Cursor{}, false, err
		}
		if len(results) == 0 {
			continue
		}
		resolvedLocations := resolveLocationsWithDump(dump, results)

		if newResultOffset := cursor.SkipResultsInDump + len(results); newResultOffset < count {
			newCursor := cursor
			newCursor.SkipResultsInDump = newResultOffset
			return resolvedLocations, newCursor, true, nil
		}

		if i+1 < len(cursor.DumpIDs) {
			newCursor := cursor
			newCursor.SkipDumpsInBatch = i + 1
			newCursor.SkipResultsInDump = 0
			return resolvedLocations, newCursor, true, nil
		}

		if cursor.SkipDumpsWhenBatching < cursor.TotalDumpsWhenBatching {
			newCursor := cursor
			newCursor.DumpIDs = []int{}
			newCursor.SkipDumpsInBatch = 0
			newCursor.SkipResultsInDump = 0
			return resolvedLocations, newCursor, true, nil
		}

		return resolvedLocations, Cursor{}, false, nil
	}

	return nil, Cursor{}, false, nil
}
