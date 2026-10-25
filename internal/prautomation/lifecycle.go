package prautomation

// lifecycle records state transitions and queues the prompts they call for.
func (r *evaluation) lifecycle() {
	record, obs := r.record, r.obs
	state := obs.State
	if record.LastState == StateMerged {
		// Merged is final. An instance cannot reopen a merged pull request,
		// so a later "open" or "closed" reading is noise.
		state = StateMerged
	}
	if record.LastState == "" {
		record.LastState = state
		r.reviewRequest(state)
		switch state {
		case StateMerged:
			record.LastLifecycle = EventMerged
		case StateClosed:
			record.LastLifecycle = EventClosed
		}
		return
	}
	r.reviewRequest(state)

	switch state {
	case StateMerged:
		if record.LastLifecycle != EventMerged {
			record.LastLifecycle = EventMerged
			if record.OnMerged {
				r.queue(EventMerged)
			}
		}
	case StateClosed:
		if record.LastLifecycle != EventClosed && record.LastLifecycle != EventMerged {
			record.LastLifecycle = EventClosed
			if record.OnClosed {
				r.queue(EventClosed)
			}
		}
	case StateOpen:
		if record.LastLifecycle == EventClosed {
			// Reopened: the next close prompts again.
			record.LastLifecycle = ""
		}
	}
	record.LastState = state
}

// reviewRequest follows the operator's review request and prompts when a new
// one appears. It is only read while the prompt is on and the pull request is
// open: a closed one reports no request, which is not the request going away,
// and a request still pending after a reopen must not prompt twice.
func (r *evaluation) reviewRequest(state PullState) {
	record, obs := r.record, r.obs
	if !record.OnReviewRequested || state != StateOpen {
		return
	}
	if record.ReviewBaselineSet && obs.OperatorRequested && !record.ReviewRequested {
		r.queue(EventReviewRequested)
	}
	record.ReviewRequested = obs.OperatorRequested
	record.ReviewBaselineSet = true
}

func (r *evaluation) queue(event string) {
	for _, pending := range r.record.Pending {
		if pending.Event == event {
			return
		}
	}
	r.record.Pending = append(r.record.Pending, Pending{Event: event, ID: r.engine.newID()})
}

// deliverPending sends queued lifecycle prompts, oldest first. A prompt that
// cannot be delivered stays queued for the next evaluation.
func (r *evaluation) deliverPending() {
	for len(r.record.Pending) > 0 {
		pending := r.record.Pending[0]
		text := lifecycleText(pending, r.obs, *r.record)
		if !r.send(text, "") {
			return
		}
		r.record.Pending = r.record.Pending[1:]
	}
	if len(r.record.Pending) == 0 {
		r.record.Pending = nil
	}
}
