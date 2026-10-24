//go:build integration

package billing

import "context"

// DrainForTest runs one worker pass (events, then contracts) for deterministic testing.
func (s *Service) DrainForTest(ctx context.Context) { s.drain(ctx) }
