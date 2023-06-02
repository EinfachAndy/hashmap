package hashmaps

import "fmt"

const (
	emptyBucket    = -1
	resizeFactor   = 2
	defaultMaxLoad = 0.8
)

type bucket[K comparable, V any] struct {
	key K
	// psl is the probe sequence length (PSL), which is the distance value from
	// the optimum insertion. -1 or `emptyBucket` signals a free slot.
	// inspired from:
	//  - https://programming.guide/robin-hood-hashing.html
	//  - https://cs.uwaterloo.ca/research/tr/1986/CS-86-14.pdf
	psl   int8
	value V
}

// RobinHood is a hash map that uses linear probing in combination with
// robin hood hashing as collision strategy. The hashmap resizes if the
// max PSL reached log2(n). This means that all operations including
// Get, Put, Remove have a worse case performance of O(log(n)). The expected
// max PSL for a full robin hood hash map is O(ln(n)), means a resizing
// happens at a expected default load of 0.8. The max load factor can be changed with `MaxLoad()`.
// The result is a good trade off between performance and memory consumption.
// Note that the performance for a open addressing hash map depends
// also on the key and value size. For higher storage sizes for the
// keys and values use a hashmap that uses another strategy like the golang std map.
type RobinHood[K comparable, V any] struct {
	buckets []bucket[K, V]
	hasher  HashFn[K]
	// length stores the current inserted elements
	length uintptr
	// capMinus1 is used for a bitwise AND on the hash value,
	// because the size of the underlying array is a power of two value
	capMinus1 uintptr
	// log2Cap is the number of extra reserved bytes at the end of the array,
	// to sparse the length check while probing.
	// Furthermore this value is the maximum possible PSL over the hash map,
	// because a grow is forced if this value will raised during the insert operation.
	log2Cap int8

	maxLoad float32
}

// go:inline
func newBucketArray[K comparable, V any](capacity uintptr) []bucket[K, V] {
	buckets := make([]bucket[K, V], capacity)
	for i := range buckets {
		buckets[i].psl = emptyBucket
	}
	return buckets
}

// NewRobinHood creates a ready to use `RobinHood` hash map with default settings.
func NewRobinHood[K comparable, V any]() *RobinHood[K, V] {
	return NewRobinHoodWithHasher[K, V](GetHasher[K]())
}

// NewRobinHoodWithHasher same as `NewRobinHood` but with a given hash function.
func NewRobinHoodWithHasher[K comparable, V any](hasher HashFn[K]) *RobinHood[K, V] {

	capacity := uintptr(4)
	log2Cap := uintptr(2)

	return &RobinHood[K, V]{
		buckets:   newBucketArray[K, V](capacity + log2Cap + 1),
		capMinus1: capacity - 1,
		log2Cap:   int8(log2Cap),
		hasher:    hasher,
		maxLoad:   defaultMaxLoad,
	}
}

// getBucket return a pointer to the bucket if the key was found.
// Furthermore the index of the underlying array and the psl is returned.
//
// Note:
//   - There exists also other search strategies like organ-pipe search
//     or smart search, where searching starts around the mean value
//     (mean, mean − 1, mean + 1, mean − 2, mean + 2, ...)
//   - Here it is used the simplest technic, which is more cache friendly and
//     does not track other metic values.
//
// go:inline
func (m *RobinHood[K, V]) getBucket(key K) (*bucket[K, V], uintptr, int8) {
	hash := m.hasher(key)
	idx := hash & m.capMinus1

	psl := int8(0)
	for ; psl <= m.buckets[idx].psl; psl++ {
		if m.buckets[idx].key == key {
			return &m.buckets[idx], idx, psl
		}
		idx++
	}
	return nil, idx, psl
}

// Get returns the value stored for this key, or false if there is no such value.
func (m *RobinHood[K, V]) Get(key K) (V, bool) {
	var v V
	current, _, _ := m.getBucket(key)
	if current == nil {
		return v, false
	}
	return current.value, true
}

// Reserve sets the number of buckets to the most appropriate to contain at least n elements.
// If n is lower than that, the function may have no effect.
func (m *RobinHood[K, V]) Reserve(n uintptr) {
	newCap := uintptr(NextPowerOf2(uint64(resizeFactor * n)))
	if (m.capMinus1 + 1) < newCap {
		m.resize(newCap)
	}
}

// go:inline
func (m *RobinHood[K, V]) grow() {
	capacity := m.capMinus1 + 1
	m.resize(capacity * resizeFactor)
}

// go:inline
func (m *RobinHood[K, V]) resize(n uintptr) {
	// extra space, that is at the same time the worse case lookup time
	log2Cap := (3 * uintptr(Log2(uint64(n)))) / 2
	newm := RobinHood[K, V]{
		capMinus1: n - 1,
		log2Cap:   int8(log2Cap),
		length:    m.length,
		buckets:   newBucketArray[K, V](n + log2Cap + 1),
		hasher:    m.hasher,
		maxLoad:   m.maxLoad,
	}

	for _, current := range m.buckets {
		if current.psl != emptyBucket {
			newm.Put(current.key, current.value)
		}
	}
	m.capMinus1 = newm.capMinus1
	m.log2Cap = newm.log2Cap
	m.buckets = newm.buckets
}

// Put maps the given key to the given value. If the key already exists its
// value will be overwritten with the new value. Signals if the key is new in the map.
func (m *RobinHood[K, V]) Put(key K, val V) bool {

	current, idx, psl := m.getBucket(key)

	// override old value
	if current != nil {
		current.value = val
		return false
	}

	m.emplace(bucket[K, V]{key: key, value: val, psl: psl}, idx)
	return true // new value
}

// emplace applies the Robin Hood creed to all following buckets until a empty is found.
// Robin Hood creed: "takes from the rich and gives to the poor".
// rich means, low psl
// poor means, higher psl
//
// The result is a normal distribution of the PSL values,
// where the expected length of the longest PSL is O(log(n))
// go:inline
func (m *RobinHood[K, V]) emplace(current bucket[K, V], idx uintptr) {

	if m.length >= m.capMinus1 || m.Load() > m.maxLoad {
		//fmt.Println("load: ", m.Load())
		m.grow()
		m.Put(current.key, current.value)
		return
	}

	for ; ; current.psl++ {
		if m.buckets[idx].psl == emptyBucket {
			// emplace the element, a valid bucket was found
			m.buckets[idx] = current
			m.length++
			return
		}
		// force resize to leave out overflow check of m.buckets
		if current.psl >= m.log2Cap {
			//fmt.Println("load: ", m.Load())
			m.grow()
			m.Put(current.key, current.value)
			return
		}
		if current.psl > m.buckets[idx].psl {
			current, m.buckets[idx] = m.buckets[idx], current // swap values
		}

		idx++
	}
}

// Remove removes the specified key-value pair from the map.
// Returns true, if the element was in the hash map.
func (m *RobinHood[K, V]) Remove(key K) bool {
	current, idx, _ := m.getBucket(key)
	if current == nil {
		return false
	}

	m.length--
	current.psl = emptyBucket // make as empty, because we want to remove it

	idx++
	next := &m.buckets[idx]

	// now, back shift all buckets until we found a optimum or empty one
	for next.psl > 0 {
		next.psl--
		*current, *next = *next, *current // swap values
		current = next
		idx++
		next = &m.buckets[idx]
	}
	return true
}

// Clear removes all key-value pairs from the map.
func (m *RobinHood[K, V]) Clear() {
	for idx := range m.buckets {
		m.buckets[idx].psl = emptyBucket
	}
}

// Load return the current load of the hash map.
func (m *RobinHood[K, V]) Load() float32 {
	return float32(m.length) / float32(len(m.buckets))
}

// MaxLoad forces resizing if the ratio is reached.
// useful values are in range [0.5-0.9]
func (m *RobinHood[K, V]) MaxLoad(lf float32) error {
	if lf <= 0.0 || lf >= 1.0 {
		return fmt.Errorf("lf must be in range (0.0 - 1.0)")
	}
	m.maxLoad = lf
	return nil
}

// Size returns the number of items in the map.
func (m *RobinHood[K, V]) Size() int {
	return int(m.length)
}

// Copy returns a copy of this map.
func (m *RobinHood[K, V]) Copy() *RobinHood[K, V] {
	newM := &RobinHood[K, V]{
		buckets:   make([]bucket[K, V], len(m.buckets)),
		capMinus1: m.capMinus1,
		log2Cap:   m.log2Cap,
		length:    m.length,
		hasher:    m.hasher,
		maxLoad:   m.maxLoad,
	}
	copy(newM.buckets, m.buckets)
	return newM
}

// Each calls 'fn' on every key-value pair in the hash map in no particular order.
// If 'fn' returns true, the iteration stops.
func (m *RobinHood[K, V]) Each(fn func(key K, val V) bool) {
	for _, current := range m.buckets {
		if current.psl != emptyBucket {
			if stop := fn(current.key, current.value); stop {
				// stop iteration
				return
			}
		}
	}
}