package core

import (
	"sync"

	"github.com/dop251/goja"
)

const computedVMPoolSize = 6

type computedPoolItem struct {
	mux  sync.Mutex
	busy bool
	vm   *goja.Runtime
}

// computedVMPool is a tiny fixed-size pool of sandboxed goja runtimes.
//
// When all prewarmed VMs are busy, a one-off throwaway VM is created,
// similar to the jsvm plugin pool behavior, allowing concurrent queries
// to proceed without serializing on a single runtime.
type computedVMPool struct {
	mux     sync.RWMutex
	factory func() *goja.Runtime
	items   []*computedPoolItem
}

func newComputedVMPool(size int, factory func() *goja.Runtime) *computedVMPool {
	if size <= 0 {
		size = computedVMPoolSize
	}

	pool := &computedVMPool{
		factory: factory,
		items:   make([]*computedPoolItem, size),
	}

	for i := 0; i < size; i++ {
		pool.items[i] = &computedPoolItem{vm: factory()}
	}

	return pool
}

// run executes call with a VM from the pool (or a throwaway one if all are busy).
func (p *computedVMPool) run(call func(vm *goja.Runtime) error) error {
	p.mux.RLock()

	var free *computedPoolItem
	for _, item := range p.items {
		item.mux.Lock()
		if item.busy {
			item.mux.Unlock()
			continue
		}
		item.busy = true
		item.mux.Unlock()
		free = item
		break
	}

	p.mux.RUnlock()

	if free == nil {
		// all pooled VMs are busy -> use a one-off VM (keeps concurrent queries unblocked)
		return call(p.factory())
	}

	execErr := call(free.vm)

	free.mux.Lock()
	// a VM that was interrupted (timeout) must not be reused because its
	// internal execution state is no longer reliable
	free.busy = false
	free.mux.Unlock()

	return execErr
}
