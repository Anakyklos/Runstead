package governor

import "time"

const (
	requestRetentionWindow   = 3 * time.Hour
	maxCompletedRequestIDs   = 4096
	attemptIDRetentionWindow = 3 * time.Hour
	maxRetainedAttemptIDs    = 4096
	taskRetentionWindow      = 3 * time.Hour
	maxRetainedTaskStates    = 1024
)

type taskState struct {
	attempts    int
	retries     int
	lastTouched time.Time
}

type requestState uint8

const (
	requestPending requestState = iota + 1
	requestActive
	requestCompleted
)

type requestRecord struct {
	state       requestState
	completedAt time.Time
}

type completedRequest struct {
	id          string
	completedAt time.Time
}

func (g *Governor) pruneRetentionLocked(now time.Time) {
	g.pruneAttemptIDsLocked(now)
	cutoff := now.Add(-requestRetentionWindow)
	kept := g.completedRequestIDs[:0]
	for _, entry := range g.completedRequestIDs {
		record, ok := g.requestIDs[entry.id]
		if !ok || record.state != requestCompleted || !record.completedAt.Equal(entry.completedAt) {
			continue
		}
		if !entry.completedAt.After(cutoff) {
			delete(g.requestIDs, entry.id)
			continue
		}
		kept = append(kept, entry)
	}
	g.completedRequestIDs = kept
	for len(g.completedRequestIDs) > maxCompletedRequestIDs {
		entry := g.completedRequestIDs[0]
		g.completedRequestIDs = g.completedRequestIDs[1:]
		if record, ok := g.requestIDs[entry.id]; ok && record.state == requestCompleted && record.completedAt.Equal(entry.completedAt) {
			delete(g.requestIDs, entry.id)
		}
	}

	taskCutoff := now.Add(-taskRetentionWindow)
	for taskID, state := range g.tasks {
		if g.taskProtectedLocked(taskID) || state.lastTouched.After(taskCutoff) {
			continue
		}
		delete(g.tasks, taskID)
	}
	for len(g.tasks) > maxRetainedTaskStates {
		oldestID := ""
		var oldest time.Time
		for taskID, state := range g.tasks {
			if g.taskProtectedLocked(taskID) {
				continue
			}
			if oldestID == "" || state.lastTouched.Before(oldest) {
				oldestID = taskID
				oldest = state.lastTouched
			}
		}
		if oldestID == "" {
			break
		}
		delete(g.tasks, oldestID)
	}
}

func (g *Governor) pruneAttemptIDsLocked(now time.Time) {
	cutoff := now.Add(-attemptIDRetentionWindow)
	for attemptID, seenAt := range g.attemptIDs {
		if seenAt.Before(cutoff) {
			delete(g.attemptIDs, attemptID)
		}
	}
	for len(g.attemptIDs) > maxRetainedAttemptIDs {
		oldestID := ""
		var oldest time.Time
		for attemptID, seenAt := range g.attemptIDs {
			if oldestID == "" || seenAt.Before(oldest) {
				oldestID = attemptID
				oldest = seenAt
			}
		}
		if oldestID == "" {
			break
		}
		delete(g.attemptIDs, oldestID)
	}
}

func (g *Governor) taskProtectedLocked(taskID string) bool {
	if g.inFlight && g.activeTaskID == taskID {
		return true
	}
	for _, entry := range g.queue {
		if entry.request.TaskID == taskID && !entry.removed {
			return true
		}
	}
	return false
}

func (g *Governor) requestSeenLocked(requestID string) bool {
	g.pruneRetentionLocked(g.clock.Now())
	_, seen := g.requestIDs[requestID]
	return seen
}

func (g *Governor) reserveRequestLocked(requestID string) {
	g.requestIDs[requestID] = requestRecord{state: requestPending}
}

func (g *Governor) markRequestActiveLocked(requestID string) {
	record, ok := g.requestIDs[requestID]
	if !ok {
		return
	}
	record.state = requestActive
	g.requestIDs[requestID] = record
}

func (g *Governor) completeRequestLocked(requestID string, completedAt time.Time) {
	record, ok := g.requestIDs[requestID]
	if !ok {
		return
	}
	record.state = requestCompleted
	record.completedAt = completedAt
	g.requestIDs[requestID] = record
	g.completedRequestIDs = append(g.completedRequestIDs, completedRequest{id: requestID, completedAt: completedAt})
}

func (g *Governor) releaseRequestLocked(requestID string) {
	delete(g.requestIDs, requestID)
}

func (g *Governor) taskLocked(taskID string) *taskState {
	state := g.tasks[taskID]
	if state == nil {
		state = &taskState{}
		g.tasks[taskID] = state
	}
	state.lastTouched = g.clock.Now()
	return state
}
