package serve

import (
	"sync"
	"testing"
	"time"
)

func TestReadRateLimitAcrossConcurrentCalls(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	var wg sync.WaitGroup
	results := make(chan bool, readsPerMinute+10)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := admitRead(home, "ai", now)
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != readsPerMinute {
		t.Fatalf("admitted %d", count)
	}
	if ok, err := admitRead(home, "other", now); err != nil || !ok {
		t.Fatalf("other caller: %v %v", ok, err)
	}
	if ok, err := admitRead(home, "ai", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("expired limit: %v %v", ok, err)
	}
}
