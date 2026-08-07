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
	"golang.org/x/text/unicode/norm"
	"strings"
)

const (
	Dim               = 384 // e5-small hidden size
	bosID             = 0   // XLM-R <s>
	eosID             = 2   // XLM-R </s>
	maxLen            = 512
	modelMinBytes     = 400_000_000
	tokenizerMinBytes = 1_000_000
)

// Embedder holds the loaded model + tokenizer. Construct once, reuse. Embed IS
// safe for concurrent callers: e.mu serializes the whole model exec, and the
// parallel distill drain (issue #22) shares ONE embedder across all miner
// goroutines and relies on that lock. Do NOT remove e.mu as "unnecessary on a
// single-threaded worker" — the worker is no longer single-threaded.
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

// sanitizeForTokenizer makes text safe for the byte-offset-indexing tokenizer.
//
// Extracted from Embed so it is testable WITHOUT the 448MB model. That matters: the test that
// guards this (a v0.7.2 critical — an NFD paste killed the long-lived MCP server) could only run
// on a machine that had fetched the model, and the model is gitignored and CI never fetches it,
// so the guard was green-by-skip on every CI run. A pure string function has no such excuse.
//
// Two transforms, both load-bearing (see Embed's comment for the panic details):
//   - ToValidUTF8 replaces non-UTF-8 bytes, e.g. latin-1 text from an imported document.
//   - NFC composition folds macOS's decomposed form, so "café" pasted on a Mac becomes the
//     same string as the precomposed "café" — the two must embed IDENTICALLY, or a pasted
//     query silently fails to match a stored observation.
//
// Order matters: composing invalid bytes is meaningless, so validate first.
func sanitizeForTokenizer(text string) string {
	return norm.NFC.String(strings.ToValidUTF8(text, "�"))
}

// Embed returns the L2-normalized, masked-mean-pooled 384-d vector for one text.
// Mirrors the verified reference pipeline exactly. It normalizes and sanitizes text before tokenizing, and converts a tokenizer panic
// into an error.
//
// Both guards are load-bearing, not defensive noise. The tokenizer indexes the input by
// byte offsets and PANICS (index/slice out of range) on two ordinary inputs: text in NFD
// form — which is what macOS filesystems and pastes produce, so "café" typed on a Mac
// crashes while the visually identical NFC "café" is fine — and any non-UTF-8 byte
// sequence (e.g. latin-1 text from an imported document). The mining path has a recover
// barrier (distill/drain.go), but the READ paths did not: `witness observations search`
// died with a raw Go stack trace, and because the MCP server is a long-lived process
// serving on a jsonrpc2 handler goroutine, a panic there killed the whole server
// mid-session — taking any staged record_observation writes with it. Normalizing to NFC
// and replacing invalid bytes makes the common cases WORK rather than merely not-crash;
// the recover is the backstop for whatever else the tokenizer dislikes.
func (e *Embedder) Embed(text string) (vec []float32, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			vec, err = nil, fmt.Errorf("embed: tokenizer panicked on this input: %v", r)
		}
	}()

	text = sanitizeForTokenizer(text)

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

	vec, err = tensors.CopyFlatData[float32](out[0])
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
