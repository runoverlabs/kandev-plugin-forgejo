package prautomation

import (
	"context"
	"sort"
	"time"
)

// Tick runs one pass over every workspace. Exported so tests drive the loop
// without waiting on the clock.
func (e *Engine) Tick(ctx context.Context) {
	if e.workspaces == nil {
		return
	}
	workspaces, err := e.workspaces(ctx)
	if err != nil {
		e.logf("prautomation: list workspaces: %v", err)
		return
	}
	e.mu.Lock()
	e.meanwhile = map[string]trackedTask{}
	e.mu.Unlock()
	tracked := map[string]trackedTask{}
	for _, workspaceID := range workspaces {
		if ctx.Err() != nil {
			e.mu.Lock()
			e.meanwhile = nil
			e.mu.Unlock()
			return
		}
		e.tickWorkspace(ctx, workspaceID, tracked)
	}
	e.mu.Lock()
	for taskID, task := range e.meanwhile {
		for _, ref := range task.refs {
			tracked[taskID] = tracked[taskID].with(task.workspaceID, ref)
		}
	}
	e.tracked, e.meanwhile = tracked, nil
	e.mu.Unlock()
}

func (e *Engine) tickWorkspace(ctx context.Context, workspaceID string, tracked map[string]trackedTask) {
	if !e.enabled(ctx, workspaceID) {
		// Switched off: nothing is observed, prompted or merged. Records
		// stay as they are and resume when the switch comes back on.
		return
	}
	byKey, settings, ok := e.reconcile(ctx, workspaceID)
	if !ok {
		return
	}
	now := e.now().UTC()
	for _, record := range byKey {
		if record.Switches.Any() || !record.Seeded {
			tracked[record.TaskID] = tracked[record.TaskID].with(workspaceID, record.Ref())
		}
	}
	for _, record := range e.dueRecords(byKey, now) {
		if ctx.Err() != nil {
			return
		}
		e.evaluate(ctx, workspaceID, record.Ref(), &settings)
	}
}

func (e *Engine) enabled(ctx context.Context, workspaceID string) bool {
	if e.gate == nil {
		return true
	}
	enabled, err := e.gate(ctx, workspaceID)
	return err == nil && enabled
}

// reconcile brings the workspace's records in line with its linked pull
// requests and returns them by key, with the workspace settings.
func (e *Engine) reconcile(ctx context.Context, workspaceID string) (map[string]Record, Settings, bool) {
	// Records before links: a record the panel creates for a pull request
	// linked after the links were read must not be taken for an unlinked one.
	// In this order such a record is not in the list, so it is left alone.
	records, err := e.store.List(ctx, workspaceID)
	if err != nil {
		e.logf("prautomation: list records in %s: %v", workspaceID, err)
		return nil, Settings{}, false
	}
	links, err := e.links.Linked(ctx, workspaceID)
	if err != nil {
		e.logf("prautomation: list links in %s: %v", workspaceID, err)
		return nil, Settings{}, false
	}
	byKey := make(map[string]Record, len(records))
	for _, record := range records {
		byKey[recordKey(record.Ref())] = record
	}
	settings, err := e.store.Settings(ctx, workspaceID)
	if err != nil {
		e.logf("prautomation: settings for %s: %v", workspaceID, err)
		return nil, Settings{}, false
	}
	linked := e.createLinked(ctx, workspaceID, links, settings, byKey)
	e.dropUnlinked(ctx, workspaceID, linked, byKey)
	return byKey, settings, true
}

// createLinked adds a record to byKey for every link that has none, and
// returns the keys of the links.
func (e *Engine) createLinked(ctx context.Context, workspaceID string, links []PullRef, settings Settings, byKey map[string]Record) map[string]bool {
	linked := make(map[string]bool, len(links))
	for _, ref := range links {
		key := recordKey(ref)
		linked[key] = true
		if _, ok := byKey[key]; ok {
			continue
		}
		// Upsert, not Put: the panel may have created the record with a
		// human's choice since the list was read.
		record, err := e.Upsert(ctx, workspaceID, ref, func(record *Record, created bool) error {
			if created && !settings.Defaults.Any() {
				// Nothing to copy, so nothing to learn about the pull request
				// first: the record starts with every switch off.
				record.Seeded = true
			}
			return nil
		})
		if err != nil {
			e.logf("prautomation: create record: %v", err)
			continue
		}
		byKey[key] = record
	}
	return linked
}

// dropUnlinked deletes the records whose pull request is no longer linked:
// automation for it ends with the link.
func (e *Engine) dropUnlinked(ctx context.Context, workspaceID string, linked map[string]bool, byKey map[string]Record) {
	for key, record := range byKey {
		if linked[key] {
			continue
		}
		if err := e.remove(ctx, workspaceID, record.Ref()); err != nil {
			e.logf("prautomation: delete record: %v", err)
		}
		delete(byKey, key)
	}
}

// dueRecords returns the records due at now, the longest overdue first,
// within the workspace's budget.
func (e *Engine) dueRecords(byKey map[string]Record, now time.Time) []Record {
	var due []Record
	for _, record := range byKey {
		if e.isDue(record, now) {
			due = append(due, record)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextDue.Before(due[j].NextDue) })
	if len(due) > budgetPerWorkspace {
		due = due[:budgetPerWorkspace]
	}
	return due
}

func (e *Engine) isDue(record Record, now time.Time) bool {
	if record.Seeded && (!record.Switches.Any() || record.Terminal()) {
		return false
	}
	// An unseeded record is due until it is seeded, at the pace its last
	// observation set: a pull request that cannot be read waits errorInterval.
	return !record.NextDue.After(now)
}
