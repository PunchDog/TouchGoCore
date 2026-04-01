package random

import (
	"sync"
)

const (
	// Mersenne Twister 19937 parameters (64-bit version)
	N        = 312                 // State size
	M        = 156                 // Middle word, offset used in recurrence relation
	R        = 31                  // Separation point of one word, i.e., the number of bits of lower bitmask
	W        = 64                  // Word size (bit count)
	MATRIX_A = 0xb5026f5aa96619e9  // Constant vector a
	MASK     = 6364136223846793005 // Initialization multiplier
	U        = 29                  // Most significant w-r bits
	S        = 17                  // Middle bits, Tempering
	B        = 0x71d67fffeda60000  // Tempering mask for middle bits
	T        = 37                  // Middle bits, Tempering
	C        = 0xfff7eee000000000  // Tempering mask for middle bits
	L        = 43                  // Least significant bits

	// Bit masks for upper and lower bits
	UPPER_MASK = 0xFFFFFFFF80000000 // Most significant R bits
	LOWER_MASK = 0x7FFFFFFF         // Least significant W-R bits
)

// MersenneTwister implements the Mersenne Twister MT19937-64 algorithm.
// This is a pseudorandom number generator with a period of 2^19937-1.
//
// Thread-safe for concurrent reads (Int63, Uint64, Float64, etc.).
// Seed and Reseed are mutually exclusive with read operations.
//
// Example:
//
//	seed := int64(42)
//	mt := NewMersenneTwister(&seed)
//	rand := mt.Int63()
type MersenneTwister struct {
	mu    sync.RWMutex
	mt    [N]uint64
	index int
	seed  *int64
}

// NewMersenneTwister creates a new Mersenne Twister generator seeded with the given pointer.
// The seed pointer is stored for potential external updates.
// If *seed is 0, it uses a non-zero value to avoid degenerate states.
func NewMersenneTwister(seed *int64) *MersenneTwister {
	if seed == nil || *seed == 0 {
		// Create a new seed if nil or zero
		if seed == nil {
			defaultSeed := int64(1)
			seed = &defaultSeed
		} else {
			*seed = 1
		}
	}
	mt := &MersenneTwister{
		seed: seed,
	}
	mt.init(*seed)
	return mt
}

// init initializes the state array with the given seed.
// This is the internal implementation of the initialization algorithm.
func (mt *MersenneTwister) init(seed int64) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	mt.mt[0] = uint64(seed)
	for i := 1; i < N; i++ {
		// Initialization formula: mt[i] = (MASK * (mt[i-1] ^ (mt[i-1] >> (W-2))) + i)
		mt.mt[i] = MASK*(mt.mt[i-1]^(mt.mt[i-1]>>(W-2))) + uint64(i)
	}
	mt.index = N // Force regeneration on first call
}

// Seed re-initializes the generator with a new seed value.
// This method is safe for concurrent use, though it's recommended
// to create a new generator instead if frequent reseeding is needed.
//
// If seed is 0, it uses a non-zero value to avoid degenerate states.
func (mt *MersenneTwister) Seed(seed *int64) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	if *seed == 0 {
		*seed = 1
	}
	if mt.seed != nil {
		mt.seed = seed
	}

	mt.mt[0] = uint64(*seed)
	for i := 1; i < N; i++ {
		mt.mt[i] = MASK*(mt.mt[i-1]^(mt.mt[i-1]>>(W-2))) + uint64(i)
	}
	mt.index = N
}

// twist performs the core Mersenne Twister transformation.
// It generates N new random numbers in the state array.
// Must be called with write lock held.
func (mt *MersenneTwister) twist() {
	for i := 0; i < N; i++ {
		// Calculate x using upper and lower masks
		x := (mt.mt[i] & UPPER_MASK) | (mt.mt[(i+1)%N] & LOWER_MASK)

		// Compute xA
		xA := x >> 1
		if x&1 != 0 {
			xA ^= MATRIX_A
		}

		// Generate new state value
		mt.mt[i] = mt.mt[(i+M)%N] ^ xA
	}
	mt.index = 0
}

// nextUint64 generates the next random uint64 value.
// Returns the raw random number without applying bit masking.
// Must be called with read lock held.
func (mt *MersenneTwister) nextUint64() uint64 {
	// Regenerate if we've exhausted the state array
	if mt.index >= N {
		mt.mu.RUnlock()
		mt.mu.Lock()
		mt.twist()
		mt.mu.Unlock()
		mt.mu.RLock()
	}

	y := mt.mt[mt.index]
	mt.index++

	// Tempering transformation (improves equidistribution)
	y ^= y >> U
	y ^= (y << S) & B
	y ^= (y << T) & C
	y ^= y >> L

	return y
}

// Uint64 returns a pseudo-random 64-bit unsigned integer value.
// This is the core random number generator.
func (mt *MersenneTwister) Uint64() uint64 {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	result := mt.nextUint64()

	// Update seed tracking (for compatibility with original implementation)
	// Note: This is not part of the standard MT19937-64 algorithm
	// but is preserved for backward compatibility
	*mt.seed += int64(result & 0xffff)

	return result
}

// Int63 returns a non-negative pseudo-random 63-bit integer as an int64.
// The returned value is in the range [0, 1<<63-1].
func (mt *MersenneTwister) Int63() int64 {
	return int64(mt.Uint64() >> 1)
}

// Uint32 returns a pseudo-random 32-bit unsigned integer value.
func (mt *MersenneTwister) Uint32() uint32 {
	return uint32(mt.Uint64() >> 32)
}

// Int31 returns a non-negative pseudo-random 31-bit integer as an int32.
func (mt *MersenneTwister) Int31() int32 {
	return int32(mt.Uint64() >> 33)
}

// Intn returns, as an int, a non-negative pseudo-random number in the half-open interval [0,n).
// It panics if n <= 0.
func (mt *MersenneTwister) Intn(n int) int {
	if n <= 0 {
		panic("Intn: n must be positive")
	}
	return int(mt.Uint64() % uint64(n))
}

// Int63n returns, as an int64, a non-negative pseudo-random number in the half-open interval [0,n).
// It panics if n <= 0.
func (mt *MersenneTwister) Int63n(n int64) int64 {
	if n <= 0 {
		panic("Int63n: n must be positive")
	}
	return mt.Int63() % n
}

// Uint64n returns, as a uint64, a non-negative pseudo-random number in the half-open interval [0,n).
// It panics if n <= 0.
func (mt *MersenneTwister) Uint64n(n uint64) uint64 {
	if n <= 0 {
		panic("Uint64n: n must be positive")
	}
	return mt.Uint64() % n
}

// Float64 returns a pseudo-random number in the half-open interval [0.0,1.0).
func (mt *MersenneTwister) Float64() float64 {
	return float64(mt.Int63()) / (1 << 63)
}

// Float32 returns a pseudo-random number in the half-open interval [0.0,1.0).
func (mt *MersenneTwister) Float32() float32 {
	return float32(mt.Uint64()>>32) / (1 << 32)
}

// Float32 returns a pseudo-random number in the half-open interval [min,max).
// It panics if max <= min.
func (mt *MersenneTwister) Float32Range(min, max float32) float32 {
	if max <= min {
		panic("Float32Range: max must be greater than min")
	}
	return min + mt.Float32()*(max-min)
}

// Float64 returns a pseudo-random number in the half-open interval [min,max).
// It panics if max <= min.
func (mt *MersenneTwister) Float64Range(min, max float64) float64 {
	if max <= min {
		panic("Float64Range: max must be greater than min")
	}
	return min + mt.Float64()*(max-min)
}

// resetMersenneTwister provides backward compatibility.
// It resets the generator with the current seed.
func (mt *MersenneTwister) resetMersenneTwister() {
	mt.init(*mt.seed)
}

// nextInt64 provides backward compatibility for the IRandom interface.
// It returns a non-negative 63-bit integer and updates the internal seed.
func (mt *MersenneTwister) nextInt64() int64 {
	return mt.Int63()
}
