package random

import (
	"math/rand"
)

type SystemRandom struct {
	r    rand.Source
	tick int64
	seed *int64
	// mux  sync.Mutex
}

func NewSystemRandom(seed *int64) *SystemRandom {
	s := new(SystemRandom)
	s.seed = seed
	s.newsource()
	return s
}

func (s *SystemRandom) newsource() {
	s.r = rand.NewSource(*s.seed)
	*s.seed += s.r.Int63() & 0xffff
}

func (s *SystemRandom) nextInt64() int64 {
	s.tick++
	if s.tick%50 == 0 {
		s.newsource()
	} else {
		*s.seed += s.r.Int63() & 0xffff
	}
	return s.r.Int63()
}
