package provider

import "sync"

// typedSyncMap provides typed access to the sync.Map operations used by the
// provider. Its zero value is ready to use; it must not be copied after use.
type typedSyncMap[K comparable, V any] struct {
	values sync.Map
}

func (m *typedSyncMap[K, V]) Store(key K, value V) {
	m.values.Store(key, value)
}

func (m *typedSyncMap[K, V]) Load(key K) (value V, ok bool) {
	raw, ok := m.values.Load(key)
	if ok && raw != nil {
		value = raw.(V)
	}
	return value, ok
}

func (m *typedSyncMap[K, V]) LoadAndDelete(key K) (value V, ok bool) {
	raw, ok := m.values.LoadAndDelete(key)
	if ok && raw != nil {
		value = raw.(V)
	}
	return value, ok
}
