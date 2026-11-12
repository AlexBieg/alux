package workers

import (
	"sync"
	"testing"
)

func TestConcurrentOneoffAllocationsGetDistinctKeys(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const n = 8
	keys := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := AllocOneoff(root)
			if err != nil {
				t.Error(err)
			}
			keys[i] = key
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Errorf("%s was handed out twice: %v", k, keys)
		}
		seen[k] = true
	}
}
