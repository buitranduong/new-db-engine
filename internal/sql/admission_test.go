package sql

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitQueued(a *admission, n int) {
	for {
		a.mu.Lock()
		w := a.waiting
		a.mu.Unlock()
		if w == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Reads never wait for a slot, even with every slot taken and a queue of
// writes; a freed slot goes to the oldest waiting write.
func TestAdmissionReadsBypass(t *testing.T) {
	a := &admission{slots: 1, minSlots: 1, maxSlots: 1}
	adm.Store(a)
	defer adm.Store(nil)

	hold := admit(ClassWrite) // the only slot
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := admit(ClassWrite)
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			rel()
		}()
		waitQueued(a, i)
	}
	for i := 0; i < 1000; i++ { // reads pass at once
		admit(ClassRead)()
	}
	hold()
	wg.Wait()
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("write grant order %v", order)
	}
	s, _ := GetAdmissionStats()
	if s.InUse != 0 || s.Waiting != 0 {
		t.Fatalf("slots leaked: %+v", s)
	}
	if s.Queued[ClassRead] != 0 || s.Served[ClassRead] != 1000 || s.Queued[ClassWrite] != 3 {
		t.Fatalf("stats %+v", s)
	}
}

// Heavy statements have their own pool: a full heavy pool does not hold up
// writes, and a full write pool does not hold up heavy statements.
func TestAdmissionPools(t *testing.T) {
	a := &admission{slots: 4, minSlots: 4, maxSlots: 4} // heavy pool: 1
	adm.Store(a)
	defer adm.Store(nil)
	h := admit(ClassHeavy)
	done := make(chan struct{})
	go func() { admit(ClassWrite)(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("write blocked by the heavy pool")
	}
	var ws []func()
	for i := 0; i < 4; i++ {
		ws = append(ws, admit(ClassWrite))
	}
	got := make(chan struct{})
	go func() { admit(ClassHeavy)(); close(got) }() // queues behind h only
	waitQueued(a, 1)
	select {
	case <-got:
		t.Fatal("second heavy statement ran with the heavy pool full")
	default:
	}
	h()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("heavy statement not granted after h released")
	}
	for _, r := range ws {
		r()
	}
}

// Slots shrink when scheduler latency is high and grow while statements
// wait and latency is low, within bounds.
func TestAdmissionAdjust(t *testing.T) {
	a := &admission{slots: 16, minSlots: 4, maxSlots: 20}
	for i := 0; i < 50; i++ {
		a.adjust(SchedTarget.Seconds() * 2)
	}
	if a.slots != 4 {
		t.Fatalf("slots after high latency: %d", a.slots)
	}
	a.adjust(0) // nobody waiting: unchanged
	if a.slots != 4 {
		t.Fatalf("slots grew without waiters: %d", a.slots)
	}
	for i := 0; i < 50; i++ { // a statement waits each time: every slot is busy
		a.waiting, a.inUse = 1, a.slots
		a.queues[ClassWrite] = []*waiter{{ready: make(chan struct{}), since: time.Now()}}
		a.adjust(0)
	}
	if a.slots != 20 {
		t.Fatalf("slots after low latency with waiters: %d", a.slots)
	}
}

// simulate models a machine with `cores` CPUs shared FIFO (as the Go
// scheduler and kernel share them without priorities) under 2x overload of
// everything but reads: closed-loop writers, heavy statements and
// transaction statements saturate the cores, while reads arrive at a fixed
// rate that alone would use ~1/4 of the cores. Each statement spends its
// service time on a core, through admit() as exec.go does. It returns, per
// class, statements finished within the deadline over statements issued.
type simResult struct {
	issued, ok [numClasses]int64
	readP99    time.Duration
}

func simulate(t *testing.T, withAdmission bool) simResult {
	const (
		cores    = 4
		svc      = 2 * time.Millisecond
		deadline = 50 * time.Millisecond
		readRate = 500 // per second: 500*2ms = 1 core of 4
		runFor   = 3 * time.Second
	)
	if withAdmission {
		adm.Store(&admission{slots: 3, minSlots: 3, maxSlots: 3}) // 3 write slots, 1 heavy
	} else {
		adm.Store(nil)
	}
	defer adm.Store(nil)

	cpu := make(chan struct{}, cores)
	burn := func(d time.Duration) { // wait for a core, hold it for d
		cpu <- struct{}{}
		time.Sleep(d)
		<-cpu
	}
	var r simResult
	var lat []time.Duration
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	do := func(c int, cost time.Duration) {
		t0 := time.Now()
		atomic.AddInt64(&r.issued[c], 1)
		rel := admit(c)
		burn(cost)
		rel()
		d := time.Since(t0)
		if d <= deadline {
			atomic.AddInt64(&r.ok[c], 1)
		}
		if c == ClassRead {
			mu.Lock()
			lat = append(lat, d)
			mu.Unlock()
		}
	}
	// saturating closed-loop load: 64 writers, 16 heavy (range/bulk) and
	// 16 transaction clients (two write statements per round)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				do(ClassWrite, svc)
			}
		}()
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				do(ClassHeavy, 4*svc)
			}
		}()
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				do(ClassWrite, svc)
				do(ClassWrite, svc)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let the queues build
	atomic.StoreInt64(&r.issued[ClassRead], 0)
	var rw sync.WaitGroup
	tick := time.NewTicker(time.Second / readRate)
	end := time.After(runFor)
loop:
	for {
		select {
		case <-tick.C:
			rw.Add(1)
			go func() { defer rw.Done(); do(ClassRead, svc) }()
		case <-end:
			break loop
		}
	}
	tick.Stop()
	rw.Wait()
	close(stop)
	wg.Wait()
	// p99
	for i := range lat { // insertion-free: simple selection by sorting
		_ = i
	}
	sortDurations(lat)
	if len(lat) > 0 {
		r.readP99 = lat[len(lat)*99/100]
	}
	return r
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// Under saturation by writes, heavy statements and transactions, reads
// are served (within 50 ms) at >= 99% with admission, and are not without
// it; the other classes keep running at reduced rate (no starvation).
func TestAdmissionReadPriorityUnderOverload(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation takes ~8 s")
	}
	base := simulate(t, false)
	got := simulate(t, true)
	share := func(r simResult, c int) float64 { return float64(r.ok[c]) / float64(max(r.issued[c], 1)) }
	t.Logf("%-10s %-22s %s", "", "without admission", "with admission")
	for c := 0; c < numClasses; c++ {
		t.Logf("%-10s ok %5d/%-5d %5.1f%%    ok %5d/%-5d %5.1f%%", ClassNames[c],
			base.ok[c], base.issued[c], 100*share(base, c), got.ok[c], got.issued[c], 100*share(got, c))
	}
	t.Logf("read p99: without %v, with %v", base.readP99, got.readP99)
	if s := share(got, ClassRead); s < 0.99 {
		t.Errorf("reads served within deadline: %.1f%%, want >= 99%%", 100*s)
	}
	if share(base, ClassRead) >= 0.99 {
		t.Errorf("model too weak: reads fine without admission (%.1f%%)", 100*share(base, ClassRead))
	}
	// controlled degradation: other classes still get CPU, none starved
	for _, c := range []int{ClassWrite, ClassHeavy} {
		if got.ok[c] == 0 && got.issued[c] > 0 {
			t.Errorf("%s starved", ClassNames[c])
		}
	}
}
