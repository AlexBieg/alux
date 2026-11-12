package markdown

import "sync"

type diagramKey struct {
	kind  string
	src   string
	width int
}

type drawn struct {
	text string
	ok   bool
}

// A live reply is re-rendered every frame, so each diagram is laid out once per width.
var diagramCache = struct {
	sync.Mutex
	m map[diagramKey]drawn
}{m: map[diagramKey]drawn{}}

const diagramCacheSize = 128

func cached(kind, src string, width int, draw func() (string, bool)) (string, bool) {
	key := diagramKey{kind, src, width}
	diagramCache.Lock()
	d, hit := diagramCache.m[key]
	diagramCache.Unlock()
	if hit {
		return d.text, d.ok
	}
	d.text, d.ok = draw()
	diagramCache.Lock()
	if len(diagramCache.m) >= diagramCacheSize {
		clear(diagramCache.m)
	}
	diagramCache.m[key] = d
	diagramCache.Unlock()
	return d.text, d.ok
}
