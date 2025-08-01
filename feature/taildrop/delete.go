// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package taildrop

import (
	"container/list"
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/syncs"
	"tailscale.com/tstime"
	"tailscale.com/types/logger"
)

// deleteDelay is the amount of time to wait before we delete a file.
// A shorter value ensures timely deletion of deleted and partial files, while
// a longer value provides more opportunity for partial files to be resumed.
const deleteDelay = time.Hour

// fileDeleter manages asynchronous deletion of files after deleteDelay.
type fileDeleter struct {
	logf  logger.Logf
	clock tstime.DefaultClock
	event func(string) // called for certain events; for testing only

	mu     sync.Mutex
	queue  list.List
	byName map[string]*list.Element

	emptySignal chan struct{} // signal that the queue is empty
	group       syncs.WaitGroup
	shutdownCtx context.Context
	shutdown    context.CancelFunc
	fs          FileOps // must be used for all filesystem operations
}

// deleteFile is a specific file to delete after deleteDelay.
type deleteFile struct {
	name     string
	inserted time.Time
}

func (d *fileDeleter) Init(m *manager, eventHook func(string)) {
	d.logf = m.opts.Logf
	d.clock = m.opts.Clock
	d.event = eventHook
	d.fs = m.opts.fileOps

	d.byName = make(map[string]*list.Element)
	d.emptySignal = make(chan struct{})
	d.shutdownCtx, d.shutdown = context.WithCancel(context.Background())

	// From a cold-start, load the list of partial and deleted files.
	// Only run this if we have ever received at least one file
	// to avoid ever touching the taildrop directory on systems (e.g., MacOS)
	// that pop up a security dialog window upon first access.
	if m.opts.State == nil {
		return
	}
	if b, _ := m.opts.State.ReadState(ipn.TaildropReceivedKey); len(b) == 0 {
		return
	}
	d.group.Go(func() {
		d.event("start full-scan")
		defer d.event("end full-scan")

		if d.fs == nil {
			d.logf("deleter: nil FileOps")
		}

		files, err := d.fs.ListFiles()
		if err != nil {
			d.logf("deleter: ListDir error: %v", err)
			return
		}
		for _, filename := range files {
			switch {
			case d.shutdownCtx.Err() != nil:
				return // terminate early
			case strings.HasSuffix(filename, partialSuffix):
				// Only enqueue the file for deletion if there is no active put.
				nameID := strings.TrimSuffix(filename, partialSuffix)
				if i := strings.LastIndexByte(nameID, '.'); i > 0 {
					key := incomingFileKey{clientID(nameID[i+len("."):]), nameID[:i]}
					m.incomingFiles.LoadFunc(key, func(_ *incomingFile, loaded bool) {
						if !loaded {
							d.Insert(filename)
						}
					})
				} else {
					d.Insert(filename)
				}
			case strings.HasSuffix(filename, deletedSuffix):
				// Best-effort immediate deletion of deleted files.
				name := strings.TrimSuffix(filename, deletedSuffix)
				if d.fs.Remove(name) == nil {
					if d.fs.Remove(filename) == nil {
						continue
					}
				}
				// Otherwise enqueue for later deletion.
				d.Insert(filename)
			}
		}
	})
}

// Insert enqueues baseName for eventual deletion.
func (d *fileDeleter) Insert(baseName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdownCtx.Err() != nil {
		return
	}
	if _, ok := d.byName[baseName]; ok {
		return // already queued for deletion
	}
	d.byName[baseName] = d.queue.PushBack(&deleteFile{baseName, d.clock.Now()})
	if d.queue.Len() == 1 && d.shutdownCtx.Err() == nil {
		d.group.Go(func() { d.waitAndDelete(deleteDelay) })
	}
}

// waitAndDelete is an asynchronous deletion goroutine.
// At most one waitAndDelete routine is ever running at a time.
// It is not started unless there is at least one file in the queue.
func (d *fileDeleter) waitAndDelete(wait time.Duration) {
	tc, ch := d.clock.NewTimer(wait)
	defer tc.Stop() // cleanup the timer resource if we stop early
	d.event("start waitAndDelete")
	defer d.event("end waitAndDelete")
	select {
	case <-d.shutdownCtx.Done():
	case <-d.emptySignal:
	case now := <-ch:
		d.mu.Lock()
		defer d.mu.Unlock()

		// Iterate over all files to delete, and delete anything old enough.
		var next *list.Element
		var failed []*list.Element
		for elem := d.queue.Front(); elem != nil; elem = next {
			next = elem.Next()
			file := elem.Value.(*deleteFile)
			if now.Sub(file.inserted) < deleteDelay {
				break // everything after this is recently inserted
			}

			// Delete the expired file.
			if name, ok := strings.CutSuffix(file.name, deletedSuffix); ok {
				if err := d.fs.Remove(name); err != nil && !os.IsNotExist(err) {
					d.logf("could not delete: %v", redactError(err))
					failed = append(failed, elem)
					continue
				}
			}
			if err := d.fs.Remove(file.name); err != nil && !os.IsNotExist(err) {
				d.logf("could not delete: %v", redactError(err))
				failed = append(failed, elem)
				continue
			}
			d.queue.Remove(elem)
			delete(d.byName, file.name)
			d.event("deleted " + file.name)
		}
		for _, elem := range failed {
			elem.Value.(*deleteFile).inserted = now // retry after deleteDelay
			d.queue.MoveToBack(elem)
		}

		// If there are still some files to delete, retry again later.
		if d.queue.Len() > 0 && d.shutdownCtx.Err() == nil {
			file := d.queue.Front().Value.(*deleteFile)
			retryAfter := deleteDelay - now.Sub(file.inserted)
			d.group.Go(func() { d.waitAndDelete(retryAfter) })
		}
	}
}

// Remove dequeues baseName from eventual deletion.
func (d *fileDeleter) Remove(baseName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if elem := d.byName[baseName]; elem != nil {
		d.queue.Remove(elem)
		delete(d.byName, baseName)
		// Signal to terminate any waitAndDelete goroutines.
		if d.queue.Len() == 0 {
			select {
			case <-d.shutdownCtx.Done():
			case d.emptySignal <- struct{}{}:
			}
		}
	}
}

// Shutdown shuts down the deleter.
// It blocks until all goroutines are stopped.
func (d *fileDeleter) Shutdown() {
	d.mu.Lock() // acquire lock to ensure no new goroutines start after shutdown
	d.shutdown()
	d.mu.Unlock()
	d.group.Wait()
}
