package parity

import (
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/bsv-blockchain/teranode/services/svp2p"
)

// legacyScoreLine is services/legacy/peer_server.go addBanScore's warning:
// "Misbehaving peer 127.0.0.1:1234 (outbound, ...): reason -- ban score increased to 20".
var legacyScoreLine = regexp.MustCompile(`Misbehaving peer (\S+) .*ban score (?:increased to|is) (\d+)`)

// Scores is the node's current misbehaviour total per peer address. svp2p
// exposes it; legacy only logs it, so its figure is the highest total logged.
func (n *nodeUnderTest) Scores() map[string]int {
	switch n.Impl {
	case Svp2p:
		if srv, ok := n.svc.(*svp2p.Server); ok {
			return srv.PeerScores()
		}
	case Legacy:
		out := make(map[string]int)

		for _, line := range n.Logger.Lines() {
			if m := legacyScoreLine.FindStringSubmatch(line); m != nil {
				if v, err := strconv.Atoi(m[2]); err == nil && v > out[m[1]] {
					out[m[1]] = v
				}
			}
		}

		return out
	}

	return nil
}

// scoreSampler keeps the highest score seen per peer while a scenario runs, so a
// peer that is disconnected for reaching the ban threshold still reports the
// total that dropped it.
type scoreSampler struct {
	mu   sync.Mutex
	max  map[string]int
	stop chan struct{}
	done chan struct{}
}

func (n *nodeUnderTest) sampleScores() *scoreSampler {
	s := &scoreSampler{max: make(map[string]int), stop: make(chan struct{}), done: make(chan struct{})}

	go func() {
		defer close(s.done)

		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()

		for {
			s.record(n.Scores())

			select {
			case <-s.stop:
				s.record(n.Scores())
				return
			case <-tick.C:
			}
		}
	}()

	return s
}

func (s *scoreSampler) record(cur map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for addr, v := range cur {
		if v > s.max[addr] {
			s.max[addr] = v
		}
	}
}

// Result stops sampling and returns the maxima keyed by peer address.
func (s *scoreSampler) Result() map[string]int {
	close(s.stop)
	<-s.done

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]int, len(s.max))
	for k, v := range s.max {
		out[k] = v
	}

	return out
}
