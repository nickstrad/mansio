package tenancy

// CacheLen is the number of cached credential answers.
func CacheLen(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cache)
}

// HoldHashSlot takes one password-hash slot until release.
func HoldHashSlot(s *Service) (release func()) {
	s.hash <- struct{}{}
	return func() { <-s.hash }
}
