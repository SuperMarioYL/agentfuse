package tokens

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/pkoukk/tiktoken-go"
)

// The two BPE vocab files tiktoken-go ever requests for the encodings
// encodingFor() selects (cl100k_base for the gpt-3.5/gpt-4 family, o200k_base
// for gpt-4o/gpt-5/o1/o3 and every non-OpenAI provider). Fetched once at
// build/dev time from openaipublic.blob.core.windows.net and embedded so the
// binary carries them — the proxy never opens a network connection to fetch a
// vocab at request time.
//
//go:embed data/cl100k_base.tiktoken
var embedCl100kBase []byte

//go:embed data/o200k_base.tiktoken
var embedO200kBase []byte

// offlineBpeLoader implements tiktoken.BpeLoader by serving the two embedded
// vocab files. tiktoken-go's default loader (defaultBpeLoader ->
// readFileCached -> readFile) does an http.Get against
// https://openaipublic.blob.core.windows.net/encodings/<enc>.tiktoken when the
// local cache is absent, and http.DefaultClient has Timeout=0 — an unbounded
// block that re-opens the v0.6 budget-starvation defect while a ledger
// reservation is held. Serving the embedded bytes keeps the per-request path
// off the network entirely (bounded CPU parse, once, then cached by
// tiktoken-go's encodingMap).
type offlineBpeLoader struct{}

func (offlineBpeLoader) LoadTiktokenBpe(blobpath string) (map[string]int, error) {
	var raw []byte
	switch {
	case strings.HasSuffix(blobpath, "cl100k_base.tiktoken"):
		raw = embedCl100kBase
	case strings.HasSuffix(blobpath, "o200k_base.tiktoken"):
		raw = embedO200kBase
	default:
		// An encoding we don't ship (p50k_base/r50k_base/p50k_edit). Return an
		// error so callers fall back to the chars/4 estimate rather than
		// falling through to an http.Get against openaipublic.blob.core.windows.net.
		return nil, fmt.Errorf("tiktoken offline loader: no embedded vocab for %q", blobpath)
	}
	return parseBpeRanks(raw)
}

// parseBpeRanks mirrors tiktoken-go's default loadTiktokenBpe parser: one
// "<base64-token> <rank>" pair per line, decoded into a token->rank map. Kept
// here so the offline loader produces byte-identical ranks to the upstream
// default loader (the accuracy harness compares against tiktoken counts).
func parseBpeRanks(contents []byte) (map[string]int, error) {
	bpeRanks := make(map[string]int)
	for _, line := range strings.Split(string(contents), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		bpeRanks[string(token)] = rank
	}
	return bpeRanks, nil
}

func init() {
	tiktoken.SetBpeLoader(offlineBpeLoader{})
}
