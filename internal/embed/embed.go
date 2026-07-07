// Package embed runs multilingual-e5-small (XLM-RoBERTa, EN/ZH) entirely in
// pure Go via the GoMLX simplego backend + a pure-Go SentencePiece tokenizer.
//
// Verified by spike (see docs): cos(go, onnxruntime) = 1.000000 on EN/ZH/unrelated;
// builds with CGO_ENABLED=0 (no libonnxruntime, no CGo); EN↔ZH cross-lingual
// margin +0.134. The tokenizer reproduces HF's subwords exactly once we add the
// XLM-R sequence wrap <s> … </s> (ids 0 … 2), which sugarme omits.
package embed

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/IngTian/witness/internal/bundle"
	"github.com/gomlx/gomlx/backends"
	_ "github.com/gomlx/gomlx/backends/simplego" // register pure-Go "go" backend
	"github.com/gomlx/gomlx/pkg/core/graph"
	"github.com/gomlx/gomlx/pkg/core/tensors"
	"github.com/gomlx/gomlx/pkg/ml/context"
	"github.com/gomlx/onnx-gomlx/onnx"
	"github.com/gomlx/onnx-gomlx/onnx/parser"
	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
)

const (
	Dim               = 384 // e5-small hidden size
	bosID             = 0   // XLM-R <s>
	eosID             = 2   // XLM-R </s>
	maxLen            = 512
	modelMinBytes     = 400_000_000
	tokenizerMinBytes = 1_000_000
)

// Embedder holds the loaded model + tokenizer. Construct once, reuse. Not safe
// for concurrent Embed calls (the worker is single-threaded; guard if shared).
type Embedder struct {
	model   onnx.Model
	ctx     *context.Context
	backend backends.Backend
	tok     *tokenizer.Tokenizer
	mu      sync.Mutex
}

// assetsDir resolves where the bundled model lives. modelDir must contain
// model.onnx + tokenizer.json. Resolution (bundle.Dir): WITNESS_ASSETS, else
// $CLAUDE_PLUGIN_ROOT/assets/e5-small, else exe-relative (so a Windows exec-form
// hook, which has no shell to export CLAUDE_PLUGIN_ROOT, still finds the model
// beside the installed binary), else the cwd-relative dev fallback.
func assetsDir() string {
	return bundle.Dir(filepath.Join("assets", "e5-small"), "WITNESS_ASSETS")
}

// AssetsDir returns the directory where model.onnx and tokenizer.json should
// live. Commands use this to explain missing-model state without loading GoMLX.
func AssetsDir() string { return assetsDir() }

// ModelReady is a cheap integrity gate for auto-start decisions. It mirrors the
// fetch script's coarse minimum-size checks so a partial download never causes
// the heavy embedder path to start and fail repeatedly.
func ModelReady() bool {
	dir := assetsDir()
	return fileAtLeast(filepath.Join(dir, "model.onnx"), modelMinBytes) &&
		fileAtLeast(filepath.Join(dir, "tokenizer.json"), tokenizerMinBytes)
}

func fileAtLeast(path string, min int64) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() >= min
}

// New loads the embedder from the assets directory.
func New() (*Embedder, error) {
	dir := assetsDir()
	modelPath := filepath.Join(dir, "model.onnx")
	tokPath := filepath.Join(dir, "tokenizer.json")

	if !ModelReady() {
		if _, err := os.Stat(modelPath); err != nil {
			return nil, fmt.Errorf("embed model not found at %s (run scripts/fetch-model.sh): %w", modelPath, err)
		}
		if _, err := os.Stat(tokPath); err != nil {
			return nil, fmt.Errorf("embed tokenizer not found at %s (run scripts/fetch-model.sh): %w", tokPath, err)
		}
		return nil, fmt.Errorf("embed model incomplete at %s (run scripts/fetch-model.sh)", dir)
	}

	model, err := parser.ParseFile(modelPath)
	if err != nil {
		return nil, fmt.Errorf("parse onnx: %w", err)
	}
	ctx := context.New()
	if err := model.VariablesToContext(ctx); err != nil {
		return nil, fmt.Errorf("load weights: %w", err)
	}
	backend, err := backends.New()
	if err != nil {
		return nil, fmt.Errorf("backend: %w", err)
	}
	tok, err := pretrained.FromFile(tokPath)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: %w", err)
	}
	return &Embedder{model: model, ctx: ctx, backend: backend, tok: tok}, nil
}

// tokenize returns XLM-R token ids for text, wrapped as <s> … </s>. e5 expects a
// task prefix; callers pass it (we use "query: " for everything — symmetric corpus).
func (e *Embedder) tokenize(text string) ([]int64, error) {
	enc, err := e.tok.EncodeSingle(text)
	if err != nil {
		return nil, err
	}
	inner := enc.GetIds()
	if len(inner) > maxLen-2 {
		inner = inner[:maxLen-2]
	}
	ids := make([]int64, 0, len(inner)+2)
	ids = append(ids, bosID)
	for _, id := range inner {
		ids = append(ids, int64(id))
	}
	ids = append(ids, eosID)
	return ids, nil
}

// Embed returns the L2-normalized, masked-mean-pooled 384-d vector for one text.
// Mirrors the verified reference pipeline exactly.
func (e *Embedder) Embed(text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ids, err := e.tokenize("query: " + text)
	if err != nil {
		return nil, err
	}
	seq := len(ids)
	mask := make([]int64, seq)
	ttype := make([]int64, seq)
	for i := range mask {
		mask[i] = 1
	}

	out := context.MustExecOnceN(e.backend, e.ctx,
		func(ctx *context.Context, in []*graph.Node) []*graph.Node {
			hidden := e.model.CallGraph(ctx, in[0].Graph(), map[string]*graph.Node{
				"input_ids":      in[0],
				"attention_mask": in[1],
				"token_type_ids": in[2],
			})[0] // [1, seq, 384]
			m := graph.ConvertDType(in[1], hidden.DType())                          // [1, seq]
			m = graph.InsertAxes(m, -1)                                             // [1, seq, 1]
			summed := graph.ReduceAndKeep(graph.Mul(hidden, m), graph.ReduceSum, 1) // [1,1,384]
			count := graph.ReduceAndKeep(m, graph.ReduceSum, 1)                     // [1,1,1]
			pooled := graph.Reshape(graph.Div(summed, count), 1, Dim)               // [1,384]
			norm := graph.L2Norm(pooled, -1)                                        // keeps dim
			return []*graph.Node{graph.Div(pooled, norm)}
		},
		[][]int64{ids}, [][]int64{mask}, [][]int64{ttype})

	vec, err := tensors.CopyFlatData[float32](out[0])
	if err != nil {
		return nil, err
	}
	return vec, nil
}

// Cosine returns cosine similarity of two equal-length vectors. Inputs are
// L2-normalized by Embed, so this is just the dot product, but we normalize
// defensively in case a caller passes raw vectors.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
