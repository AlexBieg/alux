// Package compact builds a session's compacted history from its transcript with no model call: an episode
// ledger whose older replies lose the sentences newer text already covers, judged by local embeddings.
package compact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/records"
)

// Marker opens every view; a user turn starting with it is a view, not something the user said.
const Marker = "Compacted history"

// embedTimeout bounds embedding one compaction; past it the view is built without the cut.
const embedTimeout = 2 * time.Minute

// Event is one line of a session transcript, the fields the view reads.
type Event struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
	Text   string          `json:"text"`
}

// Load reads the transcript's events since the last reset that a compaction did not cause, so a second
// compaction covers the raw history again instead of nesting the first view.
func Load(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	var all, held []Event
	reset := false
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Kind {
		case "reset":
			reset, held, all = true, all, nil
			continue
		case "user", "text", "tool":
		default:
			continue
		}
		if reset && e.Kind == "user" {
			reset = false
			if strings.HasPrefix(e.Text, Marker) {
				all, held = held, nil
				continue
			}
			held = nil
		}
		if strings.TrimSpace(e.Text+e.Name) != "" {
			all = append(all, e)
		}
	}
	return all, sc.Err()
}

// Build renders the view of ev. Vectors come from cachePath, extended by embedding what it lacks; when
// embedding fails the view is the plain ledger, and the error says why.
func Build(ev []Event, ws, cachePath string) (string, error) {
	return build(ev, ws, cachePath, false)
}

// BuildKeeping is Build plus the content rules: ids a step created or a later step uses, from any tool, and
// the newest tool results whole up to tailBudget.
func BuildKeeping(ev []Event, ws, cachePath string) (string, error) {
	return build(ev, ws, cachePath, true)
}

func build(ev []Event, ws, cachePath string, keep bool) (string, error) {
	vecs, err := vectors(ev, cachePath)
	if err != nil {
		vecs = nil
	}
	return reverseMMR(ev, ws, vecs, keep), err
}

// vectors embeds every sentence of the user turns and replies, reading and extending the cache.
func vectors(ev []Event, path string) (map[string][]float32, error) {
	cache := readVectors(path)
	var todo []string
	need := map[string]bool{}
	for _, e := range ev {
		if e.Kind != "user" && e.Kind != "text" {
			continue
		}
		for _, u := range units(e.Text) {
			if _, ok := cache[vecKey(u)]; !ok && !need[u] {
				need[u] = true
				todo = append(todo, u)
			}
		}
	}
	if len(todo) > 0 {
		emb := records.EmbedderFromEnv()
		if emb == nil {
			return nil, errEmbedOff
		}
		ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
		defer cancel()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		for k := 0; k < len(todo); k += 32 {
			batch := todo[k:min(k+32, len(todo))]
			titles := make([]string, len(batch))
			for i := range titles {
				titles[i] = "none"
			}
			vs, err := emb.Documents(ctx, titles, batch)
			if err != nil {
				return nil, err
			}
			for i, v := range vs {
				key := vecKey(batch[i])
				cache[key] = v
				if err := writeVector(f, key, v); err != nil {
					return nil, err
				}
			}
		}
	}
	out := map[string][]float32{}
	for _, e := range ev {
		if e.Kind == "user" || e.Kind == "text" {
			for _, u := range units(e.Text) {
				out[u] = cache[vecKey(u)]
			}
		}
	}
	return out, nil
}

var errEmbedOff = errors.New("embedding is off (ALUX_RECORDS_EMBED)")

func vecKey(s string) [16]byte {
	h := sha256.Sum256([]byte(s))
	var k [16]byte
	copy(k[:], h[:])
	return k
}

// The cache file is a run of records: 16-byte key, uint32 dimension count, then that many float32s.
func writeVector(f *os.File, key [16]byte, v []float32) error {
	b := make([]byte, 20+4*len(v))
	copy(b, key[:])
	binary.LittleEndian.PutUint32(b[16:], uint32(len(v)))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[20+4*i:], math.Float32bits(x))
	}
	_, err := f.Write(b)
	return err
}

func readVectors(path string) map[[16]byte][]float32 {
	m := map[[16]byte][]float32{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for len(b) >= 20 {
		var k [16]byte
		copy(k[:], b)
		n := int(binary.LittleEndian.Uint32(b[16:]))
		if len(b) < 20+4*n {
			break
		}
		v := make([]float32, n)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[20+4*i:]))
		}
		m[k] = v
		b = b[20+4*n:]
	}
	return m
}
