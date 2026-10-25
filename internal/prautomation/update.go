package prautomation

import "context"

// Update applies change to one record under its lock and stores it. A change
// that returns an error stores nothing.
func (e *Engine) Update(ctx context.Context, workspaceID string, ref PullRef, change func(*Record) error) (Record, error) {
	return e.modify(ctx, workspaceID, ref, false, func(record *Record, _ bool) error { return change(record) })
}

// Upsert is Update that starts a record for ref when there is none; created
// tells change which happened.
func (e *Engine) Upsert(ctx context.Context, workspaceID string, ref PullRef, change func(record *Record, created bool) error) (Record, error) {
	return e.modify(ctx, workspaceID, ref, true, change)
}

func (e *Engine) modify(ctx context.Context, workspaceID string, ref PullRef, create bool, change func(*Record, bool) error) (Record, error) {
	unlock, err := e.locks.lock(ctx, recordKey(ref))
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	record, found, err := e.store.Get(ctx, workspaceID, ref)
	if err != nil {
		return Record{}, err
	}
	if !found {
		if !create {
			return Record{}, ErrNotFound
		}
		record = Record{TaskID: ref.TaskID, RepositoryID: ref.RepositoryID, Number: ref.Number}
	}
	if err := change(&record, !found); err != nil {
		return Record{}, err
	}
	record.UpdatedAt = e.now().UTC()
	if err := e.store.Put(ctx, workspaceID, record); err != nil {
		return Record{}, err
	}
	e.track(workspaceID, record)
	return record, nil
}

func (e *Engine) remove(ctx context.Context, workspaceID string, ref PullRef) error {
	unlock, err := e.locks.lock(ctx, recordKey(ref))
	if err != nil {
		return err
	}
	defer unlock()
	return e.store.Delete(ctx, workspaceID, ref)
}
