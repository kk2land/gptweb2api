package gateway

import (
	"context"
	"time"
)

type dotSpaceExecutor interface {
	DotSpace(context.Context, int) (Binding, error)
}

func (m *Manager) ensureDotCalibration() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Dots.Enabled || !m.accepting || m.models[ModelDots] {
		return nil
	}
	for _, t := range m.tasks {
		if t.Calibration != "" && !t.Terminal() {
			return nil
		}
	}
	if len(m.queue) >= m.cfg.Tasks.QueueCapacity {
		return fault("queue_full", "waiting queue is full", 429)
	}
	nonce := randomID("dot_probe_")
	now := time.Now().UTC()
	t := &Task{ID: randomID("req_"), State: "queued", Created: now, Updated: now, Deadline: now.Add(min(m.cfg.TaskTimeout, 5*time.Minute)), Calibration: nonce, Request: Request{Model: ModelDots, Messages: []Message{{Role: "user", Content: "Gateway event calibration. Do not access files. Call start_task, then complete_task with succeeded and final_answer exactly: " + nonce}}}}
	if err := m.store.save(t, nil); err != nil {
		return err
	}
	m.tasks[t.ID] = t
	m.queue = append(m.queue, t.ID)
	m.signalLocked()
	return nil
}

func (m *Manager) claimDot(ctx context.Context, id, subID string) (any, error) {
	m.mu.Lock()
	t := m.tasks[id]
	if t == nil || t.Request.Model != ModelDots {
		m.mu.Unlock()
		return nil, fault("task_not_found", "no dots task with that identifier", 404)
	}
	if c, ok := m.dotClaims[id]; ok && t.DotSubscription == subID && !t.Terminal() && t.State == "running" && time.Now().Before(t.LeaseUntil) {
		out := dotClaimResult(t, c)
		m.mu.Unlock()
		return out, nil
	}
	if !m.cfg.Dots.Enabled || !m.tunnelReady || !m.mcpSeen || t.Calibration == "" && !m.models[ModelDots] {
		m.mu.Unlock()
		return nil, fault("model_not_ready", "dots has not passed event calibration", 503)
	}
	if t.State != "queued" || !time.Now().Before(t.Deadline) {
		m.mu.Unlock()
		return nil, fault("task_inactive", "task is already claimed or expired", 409)
	}
	if m.dotBusy || len(m.active) >= m.cfg.Tasks.MaxConcurrency || m.maintenance {
		m.mu.Unlock()
		return nil, fault("execution_busy", "wait for a global slot and the runtime Dot identity", 409)
	}
	spaceExecutor, ok := m.executor.(dotSpaceExecutor)
	if !ok {
		m.mu.Unlock()
		return nil, fault("space_unavailable", "Dot Space reservation is not implemented", 503)
	}
	c := Credentials{id, randomID("turn_")}
	next := *t
	next.TokenHash = digest(c.Token)
	next.State = "submitting"
	next.DotSubscription = subID
	next.LeaseUntil = minTime(time.Now().Add(90*time.Second), t.Deadline)
	if err := m.persistTransitionLocked(t, next, nil); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	slot := 0
	for m.busySlots[slot] {
		slot++
	}
	taskCtx, cancel := context.WithDeadline(m.ctx, t.Deadline)
	m.active[id] = cancel
	m.busySlots[slot] = true
	m.dotBusy = true
	m.dotClaims[id] = c
	remaining := m.queue[:0]
	for _, qid := range m.queue {
		if qid != id {
			remaining = append(remaining, qid)
		}
	}
	m.queue = remaining
	m.wg.Add(1)
	m.mu.Unlock()
	binding, err := spaceExecutor.DotSpace(ctx, slot)
	m.mu.Lock()
	if err != nil || t.Terminal() {
		if err == nil {
			err = fault("task_inactive", "task stopped during Space reservation", 409)
		}
		_ = m.endLocked(t, "failed", "space_unavailable", "Dot Space could not be verified")
		cancel()
		delete(m.active, id)
		delete(m.busySlots, slot)
		delete(m.dotClaims, id)
		m.dotBusy = false
		m.signalLocked()
		m.mu.Unlock()
		m.wg.Done()
		return nil, err
	}
	next = *t
	next.Binding = binding
	next.State = "running"
	if err = m.persistTransitionLocked(t, next, nil); err != nil {
		_ = m.endLocked(t, "failed", "storage_error", "cannot persist Dot Space binding")
		cancel()
		delete(m.active, id)
		delete(m.busySlots, slot)
		delete(m.dotClaims, id)
		m.dotBusy = false
		m.signalLocked()
		m.mu.Unlock()
		m.wg.Done()
		return nil, err
	}
	out := dotClaimResult(t, c)
	m.mu.Unlock()
	go m.waitDot(taskCtx, id, slot)
	return out, nil
}
func dotClaimResult(t *Task, c Credentials) map[string]any {
	return map[string]any{"request_id": t.ID, "turn_token": c.Token, "messages": t.Request.Messages, "workdir": t.Request.Options.Workdir, "lease_until": t.LeaseUntil, "deadline": t.Deadline, "instructions": mcpInstructions}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (m *Manager) renewDot(c Credentials) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.authorizeLocked(c, false)
	if err != nil {
		return nil, err
	}
	if t.Request.Model != ModelDots {
		return nil, fault("wrong_backend", "only dots tasks have leases", 400)
	}
	next := *t
	next.LeaseUntil = minTime(time.Now().Add(90*time.Second), t.Deadline)
	if err = m.persistTransitionLocked(t, next, nil); err != nil {
		return nil, err
	}
	return map[string]any{"accepted": true, "lease_until": t.LeaseUntil}, nil
}
func (m *Manager) waitDot(ctx context.Context, id string, slot int) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		t := m.tasks[id]
		ch := m.changed
		done := t.Terminal()
		m.mu.Unlock()
		if done {
			break
		}
		select {
		case <-ctx.Done():
			m.mu.Lock()
			_ = m.endLocked(m.tasks[id], "expired", "task_expired", "Dot task deadline elapsed")
			m.mu.Unlock()
		case <-ch:
		}
	}
	m.mu.Lock()
	m.active[id]()
	delete(m.active, id)
	delete(m.busySlots, slot)
	delete(m.dotClaims, id)
	m.dotBusy = false
	m.signalLocked()
	m.mu.Unlock()
}
