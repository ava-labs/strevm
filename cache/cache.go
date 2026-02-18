package cache

import (
	"cmp"
	"slices"
	"sync"
)

type UniformlyKeyed[K ~[32]byte, V any] struct {
	buckets [256]bucket[K, V]
}

type bucket[K comparable, V any] struct {
	sync.RWMutex
	data map[K]V
}

func NewUniformlyKeyed[K ~[32]byte, V any]() *UniformlyKeyed[K, V] {
	c := new(UniformlyKeyed[K, V])
	for i := range 256 {
		c.buckets[i].data = make(map[K]V)
	}
	return c
}

func (c *UniformlyKeyed[K, V]) Store(k K, v V) {
	b := &c.buckets[k[0]]
	b.Lock()
	b.data[k] = v
	b.Unlock()
}

func (c *UniformlyKeyed[K, V]) Load(k K) (V, bool) {
	b := &c.buckets[k[0]]
	b.RLock()
	v, ok := b.data[k]
	b.RUnlock()
	return v, ok
}

func (c *UniformlyKeyed[K, V]) Delete(ks ...K) {
	if len(ks) == 0 {
		return
	}
	slices.SortFunc(ks, func(a, b K) int {
		return cmp.Compare(a[0], b[0])
	})

	var (
		index  byte
		locked *bucket[K, V]
	)
	for _, k := range ks {
		if idx := k[0]; locked == nil || idx != index {
			if locked != nil {
				locked.Unlock()
			}
			index = idx
			locked = &c.buckets[idx]
			locked.Lock()
		}
		delete(locked.data, k)
	}
	locked.Unlock()
}

func (c *UniformlyKeyed[K, V]) Clear() {
	for i := range len(c.buckets) {
		b := &c.buckets[i]
		b.Lock()
		defer b.Unlock()
		b.data = make(map[K]V)
	}
}
