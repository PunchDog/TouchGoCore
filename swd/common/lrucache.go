package common

import (
	"sync"
)

// LRUCache 简单的 LRU（Least Recently Used）缓存实现
type LRUCache[K comparable, V any] struct {
	mu       sync.RWMutex
	capacity int
	items    map[K]*lruItem[K, V]
	head     *lruItem[K, V]
	tail     *lruItem[K, V]
}

// lruItem LRU 缓存项
type lruItem[K comparable, V any] struct {
	key   K
	value V
	prev  *lruItem[K, V]
	next  *lruItem[K, V]
}

// NewLRUCache 创建新的 LRU 缓存
func NewLRUCache[K comparable, V any](capacity int) *LRUCache[K, V] {
	return &LRUCache[K, V]{
		capacity: capacity,
		items:    make(map[K]*lruItem[K, V]),
	}
}

// Get 从缓存中获取值
func (c *LRUCache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if item, ok := c.items[key]; ok {
		c.moveToFront(item)
		return item.value, true
	}

	var zero V
	return zero, false
}

// Put 向缓存中添加值
func (c *LRUCache[K, V]) Put(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if item, ok := c.items[key]; ok {
		// 更新现有项
		item.value = value
		c.moveToFront(item)
		return
	}

	// 创建新项
	item := &lruItem[K, V]{
		key:   key,
		value: value,
	}
	c.items[key] = item
	c.addToFront(item)

	// 检查是否超过容量
	if len(c.items) > c.capacity {
		c.removeOldest()
	}
}

// Remove 从缓存中移除指定键
func (c *LRUCache[K, V]) Remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if item, ok := c.items[key]; ok {
		c.removeItem(item)
		delete(c.items, key)
	}
}

// Clear 清空缓存
func (c *LRUCache[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[K]*lruItem[K, V])
	c.head = nil
	c.tail = nil
}

// Size 返回缓存中的项数
func (c *LRUCache[K, V]) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Stats 返回缓存统计信息
func (c *LRUCache[K, V]) Stats() LRUCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return LRUCacheStats{
		Size:     len(c.items),
		Capacity: c.capacity,
	}
}

// LRUCacheStats LRU 缓存统计信息
type LRUCacheStats struct {
	Size     int // 当前大小
	Capacity int // 容量
}

// moveToFront 将项移到链表头部
func (c *LRUCache[K, V]) moveToFront(item *lruItem[K, V]) {
	c.removeItem(item)
	c.addToFront(item)
}

// addToFront 将项添加到链表头部
func (c *LRUCache[K, V]) addToFront(item *lruItem[K, V]) {
	item.prev = nil
	item.next = c.head

	if c.head != nil {
		c.head.prev = item
	}
	c.head = item

	if c.tail == nil {
		c.tail = item
	}
}

// removeItem 从链表中移除项
func (c *LRUCache[K, V]) removeItem(item *lruItem[K, V]) {
	if item.prev != nil {
		item.prev.next = item.next
	} else {
		c.head = item.next
	}

	if item.next != nil {
		item.next.prev = item.prev
	} else {
		c.tail = item.prev
	}
}

// removeOldest 移除最旧的项
func (c *LRUCache[K, V]) removeOldest() {
	if c.tail != nil {
		c.removeItem(c.tail)
		delete(c.items, c.tail.key)
	}
}
