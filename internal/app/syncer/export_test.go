package syncer

// ResumedCount reports how many transfers this puller still remembers having
// re-verified against their partial file. Tests use it to see that a transfer
// that ended is not remembered for the life of the process.
func (p *Puller) ResumedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.resumed)
}
