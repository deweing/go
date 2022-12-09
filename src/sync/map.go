// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination.
// Loads, stores, and deletes run in amortized constant time.
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, for better type safety and to make it
// easier to maintain other invariants along with the map content.
//
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow,
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint
// sets of keys. In these two cases, use of a Map may significantly reduce lock
// contention compared to a Go map paired with a separate Mutex or RWMutex.
//
// The zero Map is empty and ready for use. A Map must not be copied after first use.
//
// In the terminology of the Go memory model, Map arranges that a write operation
// “synchronizes before” any read operation that observes the effect of the write, where
// read and write operations are defined as follows.
// Load, LoadAndDelete, LoadOrStore are read operations;
// Delete, LoadAndDelete, and Store are write operations;
// and LoadOrStore is a write operation when it returns loaded set to false.
//
// 适用的场景：
// 1.读多写少：读多写少的环境下，都是从read的map去读取，不需要加锁，而写多读少的情况下，需要加锁，
// 其次存在将read数据同步到dirty的操作的可能性，大量的拷贝操作会大大降低性能
// 2.读写不同的key：sync.Map是针对key的值的原子操作，相当于加锁加载key上的，所以，多个key的读写是可以同时并发的
type Map struct {
	// 加锁的作用，保护dirty字段
	mu Mutex

	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held).
	//
	// The read field itself is always safe to load, but must only be stored with
	// mu held.
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged entry requires that the entry be copied to the dirty
	// map and unexpunged with mu held.
	//
	// read atomic类型，可以并发读取
	// 基本上可以把它看成一个安全的只读map
	// 它包含的元素可以通过原子操作更新，但是已经删除的entry就需要加锁操作了
	read atomic.Pointer[readOnly]

	// dirty contains the portion of the map's contents that require mu to be
	// held. To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	//
	// dirty 是一个非线程安全的原始map，
	// 需要加锁才能访问的元素
	// 包括所有在read字段中但未被expunged（删除）的元素以及新加的元素
	dirty map[any]*entry

	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	//
	// 每当从 read 读取失败，都会将 misses 的计数值加1
	// 当加到一定的阙值，需要将 dirty 提升为 read
	misses int
}

// readOnly is an immutable struct stored atomically in the Map.read field.
type readOnly struct {
	m map[any]*entry
	// 为true表示dirty和read中key不一致
	amended bool // true if the dirty map contains some key not in m.
}

// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
// expunged是一个任意指针，用于标记已从dirty map删除的entry
var expunged = new(any)

// An entry is a slot in the map corresponding to a particular key.
type entry struct {
	// p points to the interface{} value stored for the entry.
	//
	// If p == nil, the entry has been deleted, and either m.dirty == nil or
	// m.dirty[key] is e.
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.
	p atomic.Pointer[any]
}

func newEntry(i any) *entry {
	e := &entry{}
	e.p.Store(&i)
	return e
}

// 加载read数据
func (m *Map) loadReadOnly() readOnly {
	if p := m.read.Load(); p != nil {
		return *p
	}
	return readOnly{}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (m *Map) Load(key any) (value any, ok bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	// 1.fast path, 在read中找到，直接load()取出p
	// 2.如果在read中没有找到，并且dirty和read中的key不一致，在dirty里面查找
	if !ok && read.amended {
		// 因为dirty map不是线程安全，所以需要加上互斥锁
		m.mu.Lock()
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		// 再次检查，避免在上锁的过程中ditry map提升为read map
		read = m.loadReadOnly()
		e, ok = read.m[key]
		// 再次确认在read里面仍然没有找到
		if !ok && read.amended {
			// 从dirty获取
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			// 不管m.dirty中存不存在，都将misses计数加一
			// missLocked()中满足条件后就会提升m.dirty
			m.missLocked()
		}
		//解锁
		m.mu.Unlock()
	}
	// 3.如果在read中没有找到，dirty和read中的key一致，返回nil, false
	if !ok {
		return nil, false
	}
	//找到，加载数据
	return e.load()
}

// 从entry加载对象
func (e *entry) load() (value any, ok bool) {
	p := e.p.Load()
	// 判断数据状态，nil和expunged 都是为找到
	// p==expunged, 说明这条键值对已被删除
	if p == nil || p == expunged {
		return nil, false
	}
	return *p, true
}

// Store sets the value for a key.
func (m *Map) Store(key, value any) {
	_, _ = m.Swap(key, value)
}

// tryCompareAndSwap compare the entry with the given old value and swaps
// it with a new value if the entry is equal to the old value, and the entry
// has not been expunged.
//
// If the entry is expunged, tryCompareAndSwap returns false and leaves
// the entry unchanged.
func (e *entry) tryCompareAndSwap(old, new any) bool {
	p := e.p.Load()
	if p == nil || p == expunged || *p != old {
		return false
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if the comparison fails from the start, we shouldn't
	// bother heap-allocating an interface value to store.
	nc := new
	for {
		if e.p.CompareAndSwap(p, &nc) {
			return true
		}
		p = e.p.Load()
		if p == nil || p == expunged || *p != old {
			return false
		}
	}
}

// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return e.p.CompareAndSwap(expunged, nil)
}

// swapLocked unconditionally swaps a value into the entry.
//
// The entry must be known not to be expunged.
//
// swapLocked 将值更新到entry
// entry必须明确是不被删除的
func (e *entry) swapLocked(i *any) *any {
	return e.p.Swap(i)
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
func (m *Map) LoadOrStore(key, value any) (actual any, loaded bool) {
	// Avoid locking if it's a clean hit.
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			return actual, loaded
		}
	}

	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() {
			m.dirty[key] = e
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked()
	} else {
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i any) (actual any, loaded, ok bool) {
	p := e.p.Load()
	if p == expunged {
		return nil, false, false
	}
	if p != nil {
		return *p, true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.
	ic := i
	for {
		if e.p.CompareAndSwap(nil, &ic) {
			return i, false, true
		}
		p = e.p.Load()
		if p == expunged {
			return nil, false, false
		}
		if p != nil {
			return *p, true, true
		}
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
// LoadAndDelete 删除一个键，如果存在返回之前的值
// loaded 表示该键是否存在
func (m *Map) LoadAndDelete(key any) (value any, loaded bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	//1.如果read中没有这个key，但是dirty和read键不一致，去dirty看下
	if !ok && read.amended {
		m.mu.Lock()
		// 二次检查
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			//从dirty获取一次，需要返回值
			e, ok = m.dirty[key]
			//直接从dirty删除
			delete(m.dirty, key)
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	//2.在read里面找到，直接在entry里面删除
	if ok {
		return e.delete()
	}
	return nil, false
}

// Delete deletes the value for a key.
// 删除一个键
func (m *Map) Delete(key any) {
	m.LoadAndDelete(key)
}

// 在entry中删除
func (e *entry) delete() (value any, ok bool) {
	for {
		p := e.p.Load()
		//已经被删除了，就不需要处理了
		// 当 p == nil 时，说明这个键值对已被删除，并且 m.dirty == nil，或 m.dirty[k] 指向该 entry。
		// 当 p == expunged 时，说明这条键值对已被删除，并且 m.dirty != nil，且 m.dirty 中没有这个 key。
		if p == nil || p == expunged {
			return nil, false
		}
		//将key对应的值置为nil
		if e.p.CompareAndSwap(p, nil) {
			return *p, true
		}
	}
}

// trySwap swaps a value if the entry has not been expunged.
//
// If the entry is expunged, trySwap returns false and leaves the entry
// unchanged.
//
// trySwap 更新一个值到entry
// 如果该条目已经被删除，trySwap返回false，并保持不变
func (e *entry) trySwap(i *any) (*any, bool) {
	for {
		p := e.p.Load()
		if p == expunged {
			return nil, false
		}
		// CAS，直到更新成功
		if e.p.CompareAndSwap(p, i) {
			return p, true
		}
	}
}

// Swap swaps the value for a key and returns the previous value if any.
// The loaded result reports whether the key was present.
func (m *Map) Swap(key, value any) (previous any, loaded bool) {
	read := m.loadReadOnly()
	// 1.如果m.read存在这个键，并且这个entry没有标记为删除，尝试直接存储
	// 因为m.dirty也指向这个entry,所以m.ditry也保持最新的entry
	if e, ok := read.m[key]; ok {
		if v, ok := e.trySwap(&value); ok {
			if v == nil {
				return nil, false
			}
			return *v, true
		}
	}

	// 如果key在 m.read 不存在或者已经被标记删除，
	// 加锁
	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		//2.在read中存在，标记为删除
		if e.unexpungeLocked() { //标记成为未被删除
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			// 添加到dirty
			m.dirty[key] = e
		}
		//更新value
		if v := e.swapLocked(&value); v != nil {
			loaded = true
			previous = *v
		}
	} else if e, ok := m.dirty[key]; ok {
		//3.如果键只在m.dirty存在，更新dirty中的值
		if v := e.swapLocked(&value); v != nil {
			loaded = true
			previous = *v
		}
	} else {
		// 4.新加入的键，read和dirty都不存在
		// 如果amended==false, 说明dirty和read是一致的
		// 但是我们需要新加key到dirty里面，所以需要更新read.amended
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		// 将这个entry加入到m.dirty中
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
	return previous, loaded
}

// CompareAndSwap swaps the old and new values for key
// if the value stored in the map is equal to old.
// The old value must be of a comparable type.
func (m *Map) CompareAndSwap(key, old, new any) bool {
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		return e.tryCompareAndSwap(old, new)
	} else if !read.amended {
		return false // No existing value for key.
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	read = m.loadReadOnly()
	swapped := false
	if e, ok := read.m[key]; ok {
		swapped = e.tryCompareAndSwap(old, new)
	} else if e, ok := m.dirty[key]; ok {
		swapped = e.tryCompareAndSwap(old, new)
		// We needed to lock mu in order to load the entry for key,
		// and the operation didn't change the set of keys in the map
		// (so it would be made more efficient by promoting the dirty
		// map to read-only).
		// Count it as a miss so that we will eventually switch to the
		// more efficient steady state.
		m.missLocked()
	}
	return swapped
}

// CompareAndDelete deletes the entry for key if its value is equal to old.
// The old value must be of a comparable type.
//
// If there is no current value for key in the map, CompareAndDelete
// returns false (even if the old value is the nil interface value).
func (m *Map) CompareAndDelete(key, old any) (deleted bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// Don't delete key from m.dirty: we still need to do the “compare” part
			// of the operation. The entry will eventually be expunged when the
			// dirty map is promoted to the read map.
			//
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	for ok {
		p := e.p.Load()
		if p == nil || p == expunged || *p != old {
			return false
		}
		if e.p.CompareAndSwap(p, nil) {
			return true
		}
	}
	return false
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently (including by f), Range may reflect any
// mapping for that key from any point during the Range call. Range does not
// block other methods on the receiver; even f itself may call any method on m.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map) Range(f func(key, value any) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read := m.loadReadOnly()
	// 当read和dirty元素不一致时，直接将dirty提升为read
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		m.mu.Lock()
		read = m.loadReadOnly()
		if read.amended {
			read = readOnly{m: m.dirty}
			m.read.Store(&read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	//遍历read里面的map
	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

// misses计数值加1，
// 如果达到阙值，直接将dirty赋值给read，dirty置为nil，清空计数器
func (m *Map) missLocked() {
	m.misses++
	if m.misses < len(m.dirty) {
		return
	}
	// 直接把dirty的指针给read.m，并且设置dirty为nil，
	// 这里也就是 Store 函数的最后会调用 m.dirtyLocked的原因
	m.read.Store(&readOnly{m: m.dirty})
	m.dirty = nil
	m.misses = 0
}

// 判断dirty是否为nil
// 如果为nil，则将read里面为被删除的key拷贝到dirty里面
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read := m.loadReadOnly()
	m.dirty = make(map[any]*entry, len(read.m))
	for k, e := range read.m {
		//只拷贝没有被删除的值到dirty中
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}

// 判断一个key是否被删除，如果为nil，将其改为expunged
func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := e.p.Load()
	for p == nil {
		// 将已经删除标记为nil的数据标记为expunged
		if e.p.CompareAndSwap(nil, expunged) {
			return true
		}
		p = e.p.Load()
	}
	return p == expunged
}
