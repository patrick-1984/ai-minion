package files

import (
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// take reserves the concurrency slots of a transfer (BRD NFR-6: per Worker
// end, per user, per Hub) and counts the admission against the per-Worker
// admission rate. It returns the function that gives the slots back, once.
// A transfer holds its slots from its admission to its end.
func (s *Service) take(t *transfer, p authz.Principal) (release func(), err error) {
	now := s.now()
	user := p.UserID
	workers := []string{t.targetWorker}
	if t.callerWorker != "" {
		workers = append(workers, t.callerWorker)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= proto.MaxTransfersPerHub {
		return nil, proto.NewError(proto.CodeRateLimited, "the Hub runs as many transfers as it allows; try again later")
	}
	perWorker := map[string]int{}
	perUser := 0
	count := func(o *transfer) {
		perWorker[o.targetWorker]++
		if o.callerWorker != "" {
			perWorker[o.callerWorker]++
		}
		if user != "" && o.user == user {
			perUser++
		}
	}
	for o := range s.pending {
		count(o)
	}
	for _, w := range workers {
		if perWorker[w] >= proto.MaxTransfersPerWorker {
			return nil, proto.NewError(proto.CodeRateLimited, "the Worker runs as many transfers as it allows; try again later")
		}
	}
	if user != "" && perUser >= proto.MaxTransfersPerUser {
		return nil, proto.NewError(proto.CodeRateLimited, "you run as many transfers as the Hub allows; try again later")
	}
	for _, w := range workers {
		kept := s.admissions[w][:0]
		for _, at := range s.admissions[w] {
			if now.Sub(at) < time.Minute {
				kept = append(kept, at)
			}
		}
		s.admissions[w] = kept
		if len(kept) >= proto.MaxAdmissionsPerWorkerMinute {
			return nil, proto.NewError(proto.CodeRateLimited, "too many transfers for this Worker in the last minute; try again later")
		}
	}
	for _, w := range workers {
		s.admissions[w] = append(s.admissions[w], now)
	}
	t.user = user
	s.pending[t] = struct{}{}
	done := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !done {
			done = true
			delete(s.pending, t)
		}
	}, nil
}
