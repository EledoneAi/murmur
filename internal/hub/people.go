package hub

// onlineLocked says which laptops are connected now, to this instance or another.
func (h *Hub) onlineLocked() map[string]bool {
	online := map[string]bool{}
	for _, x := range h.holdersLocked() {
		online[x.dev] = x.online
	}
	return online
}
