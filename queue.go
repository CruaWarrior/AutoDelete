package autodelete

import (
	"container/heap"
	"fmt"
	mrand "math/rand"
	"strings"
	"sync"
	"time"
)

const (
	schedulerTimeout = 100 * time.Millisecond
	workerTimeout    = 5 * time.Second
)

// An Item is something we manage in a priority queue.
type pqItem struct {
	ch       *ManagedChannel
	nextReap time.Time // The priority of the item in the queue.
	// The index is needed by update and is maintained by the heap.Interface methods.
	index int // The index of the item in the heap.
}

// A priorityQueue implements heap.Interface and holds Items.
type priorityQueue []*pqItem

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	// We want Pop to give us the highest, not lowest, priority so we use greater than here.
	return pq[i].nextReap.Before(pq[j].nextReap)
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*pqItem)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	item.index = -1 // for safety
	*pq = old[0 : n-1]
	return item
}

func (pq priorityQueue) Peek() *pqItem {
	if len(pq) == 0 {
		return nil
	}
	return pq[0]
}

type reapWorkItem struct {
	ch   *ManagedChannel
	msgs []string
}

type workerToken struct{}

type reapQueue struct {
	items  *priorityQueue
	cond   *sync.Cond
	timer  *time.Timer
	workCh chan reapWorkItem

	// Send when a worker starts, receive when a worker quits
	controlCh chan workerToken

	curMu   sync.Mutex
	curWork map[*ManagedChannel]struct{}
}

func newReapQueue(maxWorkerCount int) *reapQueue {
	var locker sync.Mutex
	q := &reapQueue{
		items:     new(priorityQueue),
		cond:      sync.NewCond(&locker),
		timer:     time.NewTimer(0),
		workCh:    make(chan reapWorkItem),
		controlCh: make(chan workerToken, maxWorkerCount),
		curWork:   make(map[*ManagedChannel]struct{}),
	}
	go func() {
		// Signal the condition variable every time the timer expires.
		for {
			<-q.timer.C
			q.cond.Signal()
		}
	}()
	heap.Init(q.items)
	return q
}

// Update adds or inserts the expiry time for the given item in the queue.
func (q *reapQueue) Update(ch *ManagedChannel, t time.Time) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	idx := -1
	for i, v := range *q.items {
		if v.ch == ch {
			idx = i
			break
		}
	}
	if idx == -1 {
		heap.Push(q.items, &pqItem{
			ch:       ch,
			nextReap: t,
		})
	} else {
		(*q.items)[idx].nextReap = t
		heap.Fix(q.items, idx)
	}
	q.cond.Signal()
}

func (q *reapQueue) WaitForNext() *ManagedChannel {
	q.cond.L.Lock()
start:
	it := q.items.Peek()
	if it == nil {
		fmt.Println("[reap] waiting for insertion")
		q.cond.Wait()
		goto start
	}
	now := time.Now()
	if it.nextReap.After(now) {
		waitTime := it.nextReap.Sub(now)
		fmt.Println("[reap] sleeping for ", waitTime-(waitTime%time.Second))
		q.timer.Reset(waitTime + 2*time.Millisecond)
		q.cond.Wait()
		goto start
	}
	x := heap.Pop(q.items)
	q.cond.L.Unlock()
	it = x.(*pqItem)
	return it.ch
}

func (b *Bot) QueueReap(c *ManagedChannel) {
	reapTime := c.GetNextDeletionTime()
	b.reaper.Update(c, reapTime)
}

// Removes the given channel from the reaper, assuming that IsDisabled() will
// return true for the passed ManagedChannel.
func (b *Bot) CancelReap(c *ManagedChannel) {
	var zeroTime time.Time
	b.reaper.Update(c, zeroTime)
}

func (b *Bot) QueueLoadBacklog(c *ManagedChannel, didFail bool) {
	c.mu.Lock()
	loadDelay := c.loadFailures
	if didFail {
		c.loadFailures = time.Duration(int64(loadDelay)*2 + int64(mrand.Intn(int(5*time.Second))))
		loadDelay = c.loadFailures
	}
	c.mu.Unlock()

	b.loadRetries.Update(c, time.Now().Add(loadDelay))
}

func reapScheduler(q *reapQueue, workerFunc func(*reapQueue, bool)) {
	q.controlCh <- workerToken{}
	go workerFunc(q, false)

	timer := time.NewTimer(0)

	for {
		ch := q.WaitForNext()

		q.curMu.Lock()
		_, channelAlreadyBeingProcessed := q.curWork[ch]
		if !channelAlreadyBeingProcessed {
			q.curWork[ch] = struct{}{}
		}
		q.curMu.Unlock()

		if channelAlreadyBeingProcessed {
			continue
		}

		sendWorkItem(q, workerFunc, timer, reapWorkItem{ch: ch})
	}
}

func sendWorkItem(q *reapQueue, workerFunc func(*reapQueue, bool), timer *time.Timer, work reapWorkItem) {
	for {
		if !timer.Stop() {
			<-timer.C
		}
		timer.Reset(schedulerTimeout)
		select {
		case q.workCh <- work:
			return
		case <-timer.C:
			// Attempt to start a new worker, or block if we can't
			select {
			case q.controlCh <- workerToken{}:
				fmt.Printf("[reap] %p: starting new worker\n", q)
				go workerFunc(q, true)
				continue
			case q.workCh <- work:
				return
			}
		}
	}
}

func (b *Bot) loadWorker(q *reapQueue, mayTimeout bool) {
	timer := time.NewTimer(0)

	if mayTimeout {
		defer func() {
			<-q.controlCh // remove a worker token
			fmt.Printf("[reap] %p: worker exiting\n", q)
		}()
	}

	for {
		if mayTimeout {
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(workerTimeout)
		}

		select {
		case <-timer.C:
			return
		case work := <-q.workCh:
			ch := work.ch
			if ch.IsDisabled() {
				continue
			}

			err := ch.LoadBacklog()

			q.curMu.Lock()
			delete(q.curWork, ch)
			q.curMu.Unlock()

			if isRetryableLoadError(err) {
				b.QueueLoadBacklog(ch, true)
			}
		}
	}
}

func (b *Bot) reapWorker(q *reapQueue, mayTimeout bool) {
	// TODO: implement mayTimeout
	for work := range q.workCh {
		ch := work.ch
		msgs, shouldQueueBacklog, isDisabled := ch.collectMessagesToDelete()
		if isDisabled {
			continue // drop ch
		}

		fmt.Printf("[reap] %s: deleting %d messages\n", ch, len(msgs))
		count, err := ch.Reap(msgs)
		if b.handleCriticalPermissionsErrors(ch.ChannelID, err) {
			continue // drop ch
		}
		if err != nil {
			fmt.Printf("[reap] %s: deleted %d, got error: %v\n", ch, count, err)
			shouldQueueBacklog = true
		} else if count == -1 {
			fmt.Printf("[reap] %s: doing single-message delete\n", ch)
		}

		q.curMu.Lock()
		delete(q.curWork, ch)
		q.curMu.Unlock()
		b.QueueReap(ch)
		if shouldQueueBacklog {
			b.QueueLoadBacklog(ch /* didFail= */, true) // add extra delay
		}
	}
}

func isRetryableLoadError(err error) bool {
	if err == nil {
		return false
	}
	// Only error to retry is a CloudFlare HTML 429
	if strings.Contains(err.Error(), "rate limit unmarshal error") {
		return true
	}
	return false
}
