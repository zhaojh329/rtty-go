package main

// Every condition update and wakeup uses the same lock as Wait. Otherwise an
// ACK or Close between the condition check and Wait can leave it asleep forever.
func (t *Terminal) Ack(n uint16) {
	t.cond.L.Lock()
	defer t.cond.L.Unlock()
	if !t.closed.Load() {
		t.wait_ack.Add(-int32(n))
		t.cond.Broadcast()
	}
}

func (t *Terminal) WaitAck(length int) {
	t.cond.L.Lock()
	defer t.cond.L.Unlock()
	if t.closed.Load() {
		return
	}
	t.wait_ack.Add(int32(length))
	for !t.closed.Load() && t.wait_ack.Load() > t.ack_block {
		t.cond.Wait()
	}
}

func (t *Terminal) stopFlow() {
	if t.cond == nil {
		t.closed.Store(true)
		return
	}
	t.cond.L.Lock()
	defer t.cond.L.Unlock()
	t.closed.Store(true)
	t.wait_ack.Store(0)
	t.cond.Broadcast()
}
