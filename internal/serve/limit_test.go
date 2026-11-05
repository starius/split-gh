package serve

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterRate(t *testing.T) {
	l := newLimiter(5, 100)
	now := time.Now()
	var wg sync.WaitGroup
	admitted := make(chan bool, 10)
	for i := 0; i < cap(admitted); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, refusal := l.take(now)
			if refusal == "" {
				release()
			}
			admitted <- refusal == ""
		}()
	}
	wg.Wait()
	close(admitted)
	count := 0
	for ok := range admitted {
		if ok {
			count++
		}
	}
	if count != 5 {
		t.Fatalf("admitted %d of 10 in a minute", count)
	}
	if _, refusal := l.take(now.Add(59 * time.Second)); refusal == "" {
		t.Fatal("admitted a sixth request within the minute")
	}
	if release, refusal := l.take(now.Add(time.Minute)); refusal != "" {
		t.Fatalf("limit didn't expire: %s", refusal)
	} else {
		release()
	}
}

func TestLimiterSlots(t *testing.T) {
	l := newLimiter(100, 2)
	now := time.Now()
	r1, _ := l.take(now)
	r2, _ := l.take(now)
	if r1 == nil || r2 == nil {
		t.Fatal("first two requests refused")
	}
	if _, refusal := l.take(now); refusal == "" {
		t.Fatal("third concurrent request admitted")
	}
	r1()
	if r3, refusal := l.take(now); refusal != "" {
		t.Fatalf("slot not released: %s", refusal)
	} else {
		r3()
	}
	r2()
}
