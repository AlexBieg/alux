package records

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ollamaURL      = "http://127.0.0.1:11434"
	gemmaModel     = "embeddinggemma"
	embedBatch     = 32
	searchTopUp    = 64
	vectorSeeds    = 1
	similarFloor   = 0.375
	queryTimeout   = 5 * time.Second
	topUpTimeout   = 5 * time.Second
	embedKeepAlive = "30m"
)

// The table is derived and rebuildable, so it is created outside the schema version and older builds still open the store.
const embeddingTable = `CREATE TABLE IF NOT EXISTS record_embeddings (
  record_id TEXT PRIMARY KEY REFERENCES records(id),
  model     TEXT NOT NULL,
  vector    BLOB NOT NULL
) STRICT`

// Embedder turns text into unit vectors. Query and document get different prompts, which only the embedder knows.
type Embedder interface {
	Model() string
	Query(ctx context.Context, q string) ([]float32, error)
	Documents(ctx context.Context, titles, texts []string) ([][]float32, error)
}

// EmbedderFromEnv is embeddinggemma on local Ollama unless ALUX_RECORDS_EMBED is 0, false, off or no.
func EmbedderFromEnv() Embedder {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALUX_RECORDS_EMBED"))) {
	case "0", "false", "off", "no":
		return nil
	}
	return &Gemma{URL: ollamaURL, http: &http.Client{Timeout: 30 * time.Second}}
}

// Gemma is embeddinggemma behind Ollama, with the prompts its model card gives for retrieval.
type Gemma struct {
	URL  string
	http *http.Client
}

func (g *Gemma) Model() string { return gemmaModel }

func (g *Gemma) Query(ctx context.Context, q string) ([]float32, error) {
	v, err := g.embed(ctx, []string{"task: search result | query: " + q})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func (g *Gemma) Documents(ctx context.Context, titles, texts []string) ([][]float32, error) {
	in := make([]string, len(titles))
	for i := range titles {
		in[i] = "title: " + titles[i] + " | text: " + texts[i]
	}
	return g.embed(ctx, in)
}

func (g *Gemma) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": gemmaModel, "input": inputs, "keep_alive": embedKeepAlive})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s is not answering; start it with `ollama serve`", g.URL)
	}
	defer resp.Body.Close()
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s returned %s", g.URL, resp.Status)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%s: %s", gemmaModel, out.Error)
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("%s returned %d vectors for %d inputs", gemmaModel, len(out.Embeddings), len(inputs))
	}
	for _, v := range out.Embeddings {
		normalize(v)
	}
	return out.Embeddings, nil
}

func normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
}

type vectorCache struct {
	mu   sync.Mutex
	gen  string
	ids  []string
	vecs [][]float32
}

// SetEmbedder turns on vector seeds for search and starts embedding the records that have no vector yet.
func (s *Store) SetEmbedder(e Embedder) error {
	if _, err := s.db.Exec(embeddingTable); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.embed, s.stopEmbed, s.minSim = e, cancel, similarFloor
	go func() {
		for ctx.Err() == nil {
			if n, err := s.fillEmbeddings(ctx, 256); err != nil || n == 0 {
				return
			}
		}
	}()
	return nil
}

// fillEmbeddings embeds up to limit current records lacking a vector for the model, newest first.
func (s *Store) fillEmbeddings(ctx context.Context, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.title, coalesce(r.text, '') FROM current_records r
		LEFT JOIN record_embeddings e ON e.record_id = r.id AND e.model = ?
		WHERE e.record_id IS NULL ORDER BY r.rid DESC LIMIT ?`, s.embed.Model(), limit)
	if err != nil {
		return 0, err
	}
	var ids, titles, texts []string
	for rows.Next() {
		var id, title, text string
		if err := rows.Scan(&id, &title, &text); err != nil {
			rows.Close()
			return 0, err
		}
		ids, titles, texts = append(ids, id), append(titles, title), append(texts, text)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i := 0; i < len(ids); i += embedBatch {
		j := min(i+embedBatch, len(ids))
		vecs, err := s.embed.Documents(ctx, titles[i:j], texts[i:j])
		if err != nil {
			return i, err
		}
		for k, v := range vecs {
			if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO record_embeddings (record_id, model, vector) VALUES (?, ?, ?)`,
				ids[i+k], s.embed.Model(), encodeVector(v)); err != nil {
				return i + k, err
			}
		}
	}
	return len(ids), nil
}

// draftVector embeds a draft the way its record will be embedded once written, or is nil when no embedder answers.
func (s *Store) draftVector(title, text string) []float32 {
	if s.embed == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	v, err := s.embed.Documents(ctx, []string{title}, []string{text})
	if err != nil {
		return nil
	}
	return v[0]
}

// logCosine adds each candidate's cosine to the draft to its features and signals, leaving its rank and score alone.
func (s *Store) logCosine(tx *sql.Tx, cands []Candidate, draft []float32) error {
	if draft == nil {
		return nil
	}
	for i := range cands {
		var b []byte
		err := tx.QueryRow(`SELECT vector FROM record_embeddings WHERE record_id = ? AND model = ?`, cands[i].ID, s.embed.Model()).Scan(&b)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		v := decodeVector(b)
		if len(v) != len(draft) {
			continue
		}
		var sim float32
		for k := range v {
			sim += v[k] * draft[k]
		}
		cands[i].features["cosine"] = float64(sim)
		cands[i].Signals = append(cands[i].Signals, fmt.Sprintf("cosine %.2f", sim))
	}
	return nil
}

func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVector(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

func (s *Store) vectors() ([]string, [][]float32, error) {
	model := s.embed.Model()
	var gen string
	if err := s.db.QueryRow(`SELECT count(*) || ':' || coalesce(max(rowid), 0) FROM record_embeddings WHERE model = ?`, model).Scan(&gen); err != nil {
		return nil, nil, err
	}
	c := &s.vecs
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen && c.ids != nil {
		return c.ids, c.vecs, nil
	}
	rows, err := s.db.Query(`SELECT record_id, vector FROM record_embeddings WHERE model = ?`, model)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids, vecs := []string{}, [][]float32{}
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, nil, err
		}
		ids, vecs = append(ids, id), append(vecs, decodeVector(b))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	c.gen, c.ids, c.vecs = gen, ids, vecs
	return ids, vecs, nil
}

// similar returns the current records closest in meaning to the query, or a note saying why there are none.
func (s *Store) similar(query string, g *linkGraph) ([]string, string) {
	if s.embed == nil {
		return nil, ""
	}
	fill, cancel := context.WithTimeout(context.Background(), topUpTimeout)
	_, err := s.fillEmbeddings(fill, searchTopUp)
	cancel()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	q, qerr := s.embed.Query(ctx, query)
	if qerr != nil {
		return nil, fmt.Sprintf("Search by meaning is unavailable (%v), so these are keyword and link matches only.", qerr)
	}
	ids, vecs, verr := s.vectors()
	if verr != nil {
		return nil, fmt.Sprintf("Search by meaning is unavailable (%v), so these are keyword and link matches only.", verr)
	}
	type hit struct {
		id  string
		sim float32
	}
	var top []hit
	for i, v := range vecs {
		if n, ok := g.index[ids[i]]; !ok || !g.record[n] || len(v) != len(q) {
			continue
		}
		var sim float32
		for k := range q {
			sim += q[k] * v[k]
		}
		if sim < s.minSim {
			continue
		}
		if len(top) < vectorSeeds || sim > top[len(top)-1].sim {
			top = append(top, hit{ids[i], sim})
			for j := len(top) - 1; j > 0 && top[j].sim > top[j-1].sim; j-- {
				top[j], top[j-1] = top[j-1], top[j]
			}
			top = top[:min(len(top), vectorSeeds)]
		}
	}
	out := make([]string, len(top))
	for i, h := range top {
		out[i] = h.id
	}
	if err != nil {
		return out, fmt.Sprintf("Some new records are not embedded yet (%v), so search by meaning may miss them.", err)
	}
	return out, ""
}
