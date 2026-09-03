package persistence

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

var ErrNotFound = errors.New("record not found")

type collectionFile[T any] struct {
	Version  int          `json:"version"`
	Revision uint64       `json:"revision"`
	Items    map[string]T `json:"items"`
}

type Collection[T any] struct {
	mu   sync.RWMutex
	path string
	data collectionFile[T]
}

func OpenCollection[T any](path string) (*Collection[T], error) {
	collection := &Collection[T]{
		path: path,
		data: collectionFile[T]{Version: 1, Items: make(map[string]T)},
	}
	if err := ReadJSON(path, &collection.data); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return collection, nil
		}
		return nil, err
	}
	if collection.data.Version != 1 {
		return nil, fmt.Errorf("unsupported collection version %d", collection.data.Version)
	}
	if collection.data.Items == nil {
		collection.data.Items = make(map[string]T)
	}
	return collection, nil
}

func (c *Collection[T]) Put(id string, value T) error {
	if id == "" {
		return errors.New("record id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, existed := c.data.Items[id]
	c.data.Items[id] = value
	c.data.Revision++
	if err := WriteJSON(c.path, c.data, 0o600); err != nil {
		if existed {
			c.data.Items[id] = previous
		} else {
			delete(c.data.Items, id)
		}
		c.data.Revision--
		return err
	}
	return nil
}

func (c *Collection[T]) Update(id string, update func(T) (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	current, ok := c.data.Items[id]
	if !ok {
		return zero, ErrNotFound
	}
	next, err := update(current)
	if err != nil {
		return zero, err
	}
	c.data.Items[id] = next
	c.data.Revision++
	if err := WriteJSON(c.path, c.data, 0o600); err != nil {
		c.data.Items[id] = current
		c.data.Revision--
		return zero, err
	}
	return next, nil
}

func (c *Collection[T]) Get(id string) (T, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.data.Items[id]
	if !ok {
		var zero T
		return zero, ErrNotFound
	}
	return value, nil
}

func (c *Collection[T]) List() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.data.Items))
	for key := range c.data.Items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]T, 0, len(keys))
	for _, key := range keys {
		values = append(values, c.data.Items[key])
	}
	return values
}

func (c *Collection[T]) Delete(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.data.Items[id]
	if !ok {
		return ErrNotFound
	}
	delete(c.data.Items, id)
	c.data.Revision++
	if err := WriteJSON(c.path, c.data, 0o600); err != nil {
		c.data.Items[id] = current
		c.data.Revision--
		return err
	}
	return nil
}

func (c *Collection[T]) Revision() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.Revision
}
